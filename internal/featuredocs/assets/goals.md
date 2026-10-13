# Goals and plans

Goals are persistent records used for multi-step work. A goal has an objective,
status, reason, and plan items.

## Use the goal tools

- `create_goal`: create the active objective. It may include an advisory token
  budget and an initial plan.
- `get_goal`: read the complete current objective, status, reason, and plan.
- `update_goal`: change status or reason, or replace the plan. A plan-only update
  is valid.

Use `status=completed` only when the objective is finished. Use `status=blocked`
when a concrete blocker prevents progress, and explain the blocker in `reason`.
Token budgets are telemetry, not automatic blockers.

Keep the plan concrete. Each item should describe one verifiable result and use
`pending`, `in_progress`, `completed`, or `cancelled`, with `high`, `medium`, or
`low` priority.

The retired `todowrite` tool is hidden from new runs but remains executable for
replaying older sessions. New planning must use the goal tools.
