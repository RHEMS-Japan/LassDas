# Working rules (sample)

This file is copied into every agent's home before it runs (see
`agents.*.knowledge.rules` in `config/m1-consumer.json`), so the agents obey
the same standing instructions your own engineers do.

Replace this sample with the rules your organization already uses. Typical
content:

- What counts as "done" (verification requirements, evidence expectations)
- Reporting style and language
- Hard prohibitions (destructive commands, files that must not be touched)

Keep it self-contained: agents run in a clean checkout with no access to your
laptops, wikis, or chat history.
