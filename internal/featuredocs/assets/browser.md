# Browser features

Ceraa offers one tool per browser action. Use the action that matches the
current step instead of the retired combined `browser` tool.

## Normal sequence

1. `browser_open` a complete `http` or `https` URL.
2. `browser_eval` or `browser_screenshot` to inspect the page.
3. `browser_click`, `browser_type`, or another action on the current page.
4. Inspect again after actions that change the page.
5. `browser_close` when the browser is no longer needed.

The browser keeps a page between actions in one run. Do not assume a page is
still open after a new run or a process restart.

## Practical rules

- Use `browser_open` for navigation. Do not use `browser_type` to enter a URL.
- Use CSS selectors for clicks and typing.
- Use `browser_eval` for DOM inspection or small page-local scripts.
- Use screenshots when visual state matters.
- Give each action its own call and wait for the result before choosing the next.
- The browser refuses unsafe URL schemes. Use normal web URLs only.

The old combined `browser` name remains executable only for replaying historical
sessions. New work should use the action-specific names.
