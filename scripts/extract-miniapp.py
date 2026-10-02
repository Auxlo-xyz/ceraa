#!/usr/bin/env python3
"""Extract a precise inventory of miniapp/index.html: DOM tree, CSS rules,
tokens, and the elements the script creates at runtime.

Run from the repository root:  python3 scripts/extract-miniapp.py

Line numbers are 1-based against the file as it stands, so an edit made after
this ran has to be followed by another run for them to still be right. A
snapshot an editor cannot refresh is worse than none.
"""
import json, os, re, sys
from html.parser import HTMLParser

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SRC = sys.argv[1] if len(sys.argv) > 1 else os.path.join(ROOT, "miniapp", "index.html")
OUT = os.path.join(ROOT, "docs", "miniapp-elements.json")
lines = open(SRC, encoding="utf-8").read().split("\n")
text = "\n".join(lines)

VOID = {"area","base","br","col","embed","hr","img","input","link","meta","param","source","track","wbr"}
# SVG children are parsed loosely by HTMLParser but the markup is all well formed.

class Node:
    def __init__(self, tag, attrs, line, parent):
        self.tag = tag; self.attrs = attrs; self.line = line; self.parent = parent
        self.children = []; self.text_parts = []
    @property
    def id(self): return self.attrs.get("id")
    @property
    def classes(self): return (self.attrs.get("class") or "").split()

class P(HTMLParser):
    def __init__(self):
        super().__init__(convert_charrefs=False)
        self.root = Node("#document", {}, 1, None)
        self.stack = [self.root]
    def handle_starttag(self, tag, attrs):
        a = {k: (v if v is not None else "") for k, v in attrs}
        n = Node(tag, a, self.getpos()[0], self.stack[-1])
        self.stack[-1].children.append(n)
        if tag not in VOID:
            self.stack.append(n)
    def handle_startendtag(self, tag, attrs):
        a = {k: (v if v is not None else "") for k, v in attrs}
        self.stack[-1].children.append(Node(tag, a, self.getpos()[0], self.stack[-1]))
    def handle_endtag(self, tag):
        for i in range(len(self.stack) - 1, 0, -1):
            if self.stack[i].tag == tag:
                del self.stack[i:]
                return
    def handle_data(self, d):
        s = d.strip()
        if s and self.stack[-1].tag != "script" and self.stack[-1].tag != "style":
            self.stack[-1].text_parts.append(s)

p = P()
# Only parse the markup: hand the parser the body slice so <style>/<script>
# bodies do not confuse the tree.
b0 = text.index("<body>")
b1 = text.index("<script>", b0)
p.feed(text[b0:b1])
p.close()

def serialize(n, depth=0):
    out = {
        "tag": n.tag,
        "line": n.line,
    }
    if n.id: out["id"] = n.id
    if n.classes: out["class"] = " ".join(n.classes)
    for k, v in n.attrs.items():
        if k in ("id", "class"): continue
        out.setdefault("attrs", {})[k] = v
    if n.text_parts and not n.children:
        t = " ".join(" ".join(n.text_parts).split())
        if t: out["text"] = t[:80]
    if n.children:
        out["children"] = [serialize(c, depth + 1) for c in n.children]
    return out

dom = serialize(p.root)

# ---------------------------------------------------------------- CSS
style_start = text.index("<style>")
style_end = text.index("</style>")
css_text = text[style_start + len("<style>"):style_end]
css_base_line = text[:style_start].count("\n") + 1

def clean_comments(s):
    return re.sub(r"/\*.*?\*/", "", s, flags=re.S)

def line_of(pos):
    return css_base_line + css_text.count("\n", 0, pos)

rules = []
def parse_css(s, base=0):
    def line_of(pos):
        return css_base_line + css_text.count("\n", 0, base + pos)
    i = 0
    pending = []
    out = []
    while i < len(s):
        # comment
        m = re.compile(r"/\*.*?\*/", re.S).match(s, i)
        if m:
            c = " ".join(m.group(0)[2:-2].split())
            if c: pending.append(c)
            i = m.end(); continue
        if s[i] in " \t\r\n": i += 1; continue
        # find either an at-rule block or a plain rule
        j, brace = i, False
        while j < len(s):
            ch = s[j]
            if ch == "{":
                brace = True; break
            if ch == ";":
                break
            j += 1
        head = " ".join(s[i:j].split())
        if brace:
            k, d = j + 1, 1
            while k < len(s) and d:
                if s[k] == "{": d += 1
                elif s[k] == "}": d -= 1
                k += 1
            body = s[j + 1:k - 1]
            if head.startswith("@"):
                name = head.split()[0]
                if name in ("@media", "@supports"):
                    out.append({"kind": "at-block", "at": head, "line": line_of(i),
                                "comment": pending, "rules": parse_css(body, base + j + 1)})
                elif name == "@keyframes":
                    frames = []
                    fi = 0
                    while fi < len(body):
                        if body[fi] in " \t\r\n": fi += 1; continue
                        fc = body.index("{", fi)
                        fs = " ".join(body[fi:fc].split())
                        fdepth = 1
                        fk = fc + 1
                        while fk < len(body) and fdepth:
                            if body[fk] == "{": fdepth += 1
                            elif body[fk] == "}": fdepth -= 1
                            fk += 1
                        frames.append({"selector": fs, "declarations": decls(body[fc + 1:fk - 1])})
                        fi = fk
                    out.append({"kind": "keyframes", "at": head, "line": line_of(i),
                                "comment": pending, "frames": frames})
                else:
                    out.append({"kind": "at-block", "at": head, "line": line_of(i),
                                "comment": pending, "declarations": decls(body)})
            else:
                for sel in split_selectors(head):
                    out.append({"kind": "rule", "selector": sel, "line": line_of(i),
                                "lineEnd": line_of(j), "comment": pending, "declarations": decls(body)})
            i = k
        else:
            if head.startswith("@"):
                out.append({"kind": "at-statement", "at": head, "line": line_of(i), "comment": pending})
            i = j + 1
        pending = []
    return out

def decls(body):
    b = clean_comments(body)
    out = []
    for part in split_decls(b):
        part = part.strip()
        if not part or ":" not in part: continue
        k, v = part.split(":", 1)
        k = " ".join(k.split()); v = " ".join(v.split()).rstrip(";").strip()
        if k: out.append([k, v])
    return out

def split_decls(b):
    out, depth, cur = [], 0, []
    for ch in b:
        if ch == "(": depth += 1
        elif ch == ")": depth -= 1
        if ch == ";" and depth == 0:
            out.append("".join(cur)); cur = []
        else:
            cur.append(ch)
    out.append("".join(cur))
    return out

def split_selectors(head):
    parts, depth, cur = [], 0, []
    for ch in head:
        if ch == "(": depth += 1
        elif ch == ")": depth -= 1
        if ch == "," and depth == 0:
            parts.append("".join(cur)); cur = []
        else:
            cur.append(ch)
    parts.append("".join(cur))
    return [" ".join(p.split()) for p in parts if p.strip()]

css = parse_css(css_text)

# ---------------------------------------------------------------- tokens
def grab_block(start_marker):
    m = re.search(re.escape(start_marker), text)
    i = m.start(); depth = 0; j = i
    while j < len(text):
        if text[j] == "{": depth += 1
        elif text[j] == "}":
            depth -= 1
            if depth == 0: break
        j += 1
    body = text[i:j + 1]
    return decls(body), text[:i].count("\n") + 1

dark_tok, dark_line = grab_block(":root {")
light_tok, light_line = grab_block("@media (prefers-color-scheme: light)")


# ---------------------------------------------------------------- JS analysis
js_start = text.index("<script>") + len("<script>")
js = text[js_start:]
# jsline maps a position in js to the file line a text editor shows. js begins
# immediately after the "<script>" tag, which sits on its own line, so the line
# js[0] belongs to is the tag's line. Derive it from the tag offset rather than
# from js_start so an inline "<script>" cannot skew every reported line.
js_start_line = text[:text.index("<script>")].count("\n") + 1
def jsline(pos):
    return js_start_line + js[:pos].count("\n")

el_calls = []
for m in re.finditer(r'(?<![A-Za-z0-9_])el\(\s*"([^"]+)"\s*(?:,\s*"([^"]*)")?', js):
    el_calls.append({"line": jsline(m.start()), "tag": m.group(1),
                     "class": m.group(2) if m.group(2) is not None else None})

creates = []
for m in re.finditer(r'createElement\("([^"]+)"\)', js):
    tail = js[m.start():m.start() + 400]
    cls = re.search(r'className\s*=\s*"([^"]*)"', tail)
    eid = re.search(r'\.id\s*=\s*"([^"]*)"', tail)
    creates.append({"line": jsline(m.start()), "tag": m.group(1),
                    "class": cls.group(1) if cls else None,
                    "id": eid.group(1) if eid else None})

datasets = {}
for m in re.finditer(r'dataset\.(\w+)\s*=', js):
    datasets.setdefault(m.group(1), []).append(jsline(m.start()))

classNameWrites = [{"line": jsline(m.start()), "value": m.group(1)}
                   for m in re.finditer(r'className\s*=\s*"([^"]*)"', js)]

textContentWrites = [{"line": jsline(m.start()), "value": m.group(1)[:60]}
                     for m in re.finditer(r'textContent\s*=\s*"([^"]*)"', js)]

data_attrs = sorted(set(re.findall(r'data-([\w-]+)\s*=', text)) |
                    set(re.findall(r'\[data-([\w-]+)\s*[=~|^$*]?\]?', text)))
state_values = sorted(set(re.findall(r'data-state="([^"]+)"', text)))
# the tool card's data-s comes from a ternary inside setToolState, not a literal
_st = re.search(r'function setToolState\(.*?\n  \}', js, re.S)
tool_s_values = sorted(set(re.findall(r'"(running|done|error|stopping|waiting)"',
                                      _st.group(0) if _st else "")))
md_classes = sorted(set(re.findall(r'class=\\?"(mh\d|mq|mhr)', js)))

# functions defined in the script, with their line and body line span
funcs = []
# [ \t]* and not \s*: \s* would happily consume the newline before the
# declaration and anchor the match on the blank line above it, which reports
# every function one line high.
for m in re.finditer(r'^[ \t]*function\s+(\w+)\s*\(', js, re.M):
    name = m.group(1)
    ln = jsline(m.start())
    # find the matching brace of the body
    j = js.index("{", m.end())
    depth, k = 1, j + 1
    while k < len(js) and depth:
        if js[k] == "{": depth += 1
        elif js[k] == "}": depth -= 1
        k += 1
    funcs.append({"name": name, "line": ln, "endLine": jsline(k)})

# top-level constants an editor may want
consts = [{"name": m.group(1), "line": jsline(m.start()), "value": m.group(2)}
           for m in re.finditer(r'^[ \t]*var\s+(\w+)\s*=\s*(\{[^{}]*\}|\[[^\]]*\]|"[^"]*")\s*;', js, re.M)]

# named lookup maps whose keys drive data-* attributes
def js_object(name):
    m = re.search(r'var\s+' + name + r'\s*=\s*\{(.*?)\}\s*;', js, re.S)
    if not m: return None, None
    pairs = {}
    for k, v in re.findall(r'(\w+|"")\s*:\s*"([^"]*)"', m.group(1)):
        pairs[k.strip('"')] = v
    return jsline(m.start()), pairs

phase_line, PHASE = js_object("PHASE")
toolword_line, TOOL_WORD = js_object("TOOL_WORD")
# mh classes come from heading length: mh1..mh4 built as 'class="mh'+len+'"'
md_classes = sorted(set(re.findall(r"class=\\?\"mh'\s*\+", js)) and
                    [f"mh{i}" for i in range(1, 5)] or []) + ["mq", "mhr"]

payload = {
    "source": SRC,
    "generatedBy": "python3 scripts/extract-miniapp.py",
    "dom": dom,
    "css": css,
    "tokens": {"dark": dark_tok, "darkLine": dark_line,
               "light": light_tok, "lightLine": light_line},
    "js": {"elCalls": el_calls, "creates": creates, "datasets": datasets,
           "classNameWrites": classNameWrites, "textContentWrites": textContentWrites,
           "dataAttrs": data_attrs, "stateValues": state_values,
           "toolSValues": tool_s_values, "mdClasses": md_classes,
           "PHASE": {"line": phase_line, "map": PHASE},
           "TOOL_WORD": {"line": toolword_line, "map": TOOL_WORD},
           "toolStateRegex": "running | done | error (setToolState, line 781)",
           "functions": funcs, "consts": consts},
}

# The rendered reference comes from a headless-Chrome capture of the live page,
# not from reading the CSS back. It is kept beside the source data rather than
# regenerated here because it needs a browser and a running surface; when it is
# absent the file is still a complete description of the source.
rendered_path = os.environ.get("MINIAPP_COMPUTED")
if rendered_path and os.path.exists(rendered_path):
    payload["rendered"] = json.load(open(rendered_path))

json.dump(payload, open(OUT, "w"), indent=1)

print("dom nodes:", json.dumps(dom).count('"tag"'))
print("css rules:", sum(1 for r in css if r["kind"] == "rule"),
      "keyframes:", sum(1 for r in css if r["kind"] == "keyframes"),
      "at-blocks:", sum(1 for r in css if r["kind"] == "at-block"))
print("el() call sites:", len(el_calls))
print("createElement sites:", len(creates))
print("functions:", len(funcs))
print("state vocab (data-state in markup/css):", state_values)
print("tool data-s vocab:", tool_s_values)
print("md classes:", md_classes)
print("data attrs:", data_attrs)
print("datasets written:", {k: len(v) for k, v in datasets.items()})
print("tokens dark:", len(dark_tok), "light:", len(light_tok))
