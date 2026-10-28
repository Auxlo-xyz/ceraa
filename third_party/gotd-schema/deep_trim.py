import re, os, shutil

SCHEMA='/workspace/gotmp/telegram-trim.tl'
OUT='/workspace/gotmp/telegram-deep.tl'
shutil.copy(SCHEMA, '/workspace/gotmp/telegram-full.bak.tl')

builtin={'int','long','int256','int128','double','bytes','string','true','#','X','Type','Hash','Natural','Bool'}

def refs_from_args(args):
    out=[]
    for tok in args.split():
        if re.match(r'^flags[0-9]*#$',tok): continue
        if ':' in tok: tok=tok.split(':',1)[1]
        tok=re.sub(r'^flags[0-9]*\.[0-9]+\?','',tok)
        while True:
            m=re.search(r'[Vv]ector<([^<>]+)>',tok)
            if not m: break
            out.append(m.group(1)); tok=tok[:m.start()]+m.group(1)+tok[m.end():]
        tok=re.sub(r'flags[0-9]*\.[0-9]+\?','',tok).strip('%!').strip('#')
        if tok and tok!='true': out.append(tok)
    return out

def refs_of(args,res):
    return refs_from_args(args)+refs_from_args(res)

pat=re.compile(r'\.(\w+)\(')
referenced=set()
for dp,_,fns in os.walk('/workspace/development/ceraa'):
    if '/.git' in dp: continue
    for fn in fns:
        if not fn.endswith('.go') or fn.endswith('_test.go'): continue
        for mm in pat.finditer(open(os.path.join(dp,fn),errors='ignore').read()):
            referenced.add(mm.group(1))
gotd='/workspace/go/pkg/mod/github.com/gotd/td@v0.163.0'
for dp,_,fns in os.walk(gotd):
    if '/tg/' in dp: continue
    for fn in fns:
        if not fn.endswith('.go') or fn.endswith('_test.go'): continue
        for mm in pat.finditer(open(os.path.join(dp,fn),errors='ignore').read()):
            referenced.add(mm.group(1))
print('referenced names:',len(referenced))

lines=open(SCHEMA).read().splitlines()
defs=[]; section='types'
for i,l in enumerate(lines):
    s=l.strip()
    m=re.match(r'^---(\w+)---$',s)
    if m: section=m.group(1); defs.append(('meta',None,None,section,i)); continue
    mm=re.match(r'^(?:!)?([a-zA-Z0-9_.]+)(#[0-9a-fA-F]+)?\s*(.*?)\s*=\s*(.+?);(?:\s*//.*)?$',s)
    if not mm: defs.append(('meta',None,None,section,i)); continue
    defs.append(('def',mm.group(1),(mm.group(3),mm.group(4)),section,i))

funcs=[d for d in defs if d[0]=='def' and d[3]=='functions']
print('functions:',len(funcs))

def keep_fn(pred):
    if '.' not in pred: return True
    tail=pred.split('.',1)[1]
    return (tail[0].upper()+tail[1:]) in referenced

drop_fns=set(d[1] for d in funcs if not keep_fn(d[1]))
print('functions to drop:',len(drop_fns))

# forward closure from the kept functions: a class survives only if a kept def references it
alive=dict((d[1],True) for d in defs if d[0]=='def' and d[1] not in drop_fns)
needed=set(); changed=True
while changed:
    changed=False
    for d in defs:
        if d[0]!='def' or d[1] not in alive: continue
        for t in refs_of(*d[2]):
            if t in builtin or t.startswith('flags'): continue
            if t in needed: continue
            needed.add(t); changed=True
    # constructors of needed classes become alive, pulling in their refs next round
    for d in defs:
        if d[0]=='def' and d[1] not in alive and d[2][1].strip() in needed:
            alive[d[1]]=True; changed=True
dead=set(d[2][1].strip() for d in defs if d[0]=='def' and d[2][1].strip() not in needed)
print('dead classes:',len(dead))

removed=set(drop_fns)
for d in defs:
    if d[0]=='def' and d[1] not in alive:
        removed.add(d[1])
print('cascade removed:',len(removed))

# validation: every class referenced by a kept def must have a surviving constructor
changed=True
while changed:
    changed=False
    alive=[d for d in defs if d[0]=='def' and d[1] not in removed]
    have=set(d[2][1].strip() for d in alive)
    need=set()
    for d in alive:
        for t in refs_of(*d[2]):
            if t in builtin or t.startswith('flags'): continue
            need.add(t)
    missing=need-have
    if missing:
        for d in defs:
            if d[0]=='def' and d[1] in removed and d[2][1].strip() in missing:
                removed.discard(d[1]); changed=True
print('final removed:',len(removed))

out=[]; pending=[]
for d in defs:
    i=d[4]; l=lines[i]
    if d[0]=='meta':
        if l.strip().startswith('//'): pending.append(l)
        else: out.append(l); pending=[]
        continue
    if d[1] in removed:
        pending=[]; continue
    out.extend(pending); pending=[]; out.append(l)
out.extend(pending)
open(OUT,'w').write('\n'.join(out)+'\n')
kept=sum(1 for d in defs if d[0]=='def' and d[1] not in removed)
print('kept defs:',kept)
