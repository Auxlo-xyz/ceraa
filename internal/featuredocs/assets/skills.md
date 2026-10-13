# Skills

Skills are reusable approved guidance that can be loaded when a task needs it.
The normal model-facing catalog lists approved skill names, not every skill body.

## Use

1. Use `skill_use` with the approved skill name.
2. Read the returned instructions before applying the skill.
3. Follow the skill's scope and validation requirements.

Skill content is versioned. User feedback can create a proposal through
`skill_propose`, but a proposal does not change the active skill until a person
approves it. Telegram provides approval, rejection, and rollback controls.

Do not treat an unapproved proposal as active guidance. If a requested skill is
not approved, use the normal tools and report that it is unavailable.

Use `/skills_catalog` in Telegram to inspect the available skill feature and
`/skill` to manage proposals.
