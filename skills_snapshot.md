The following skills are available for use:

- cavecrew: Decision guide for delegating to caveman-style subagents. Tells the main thread WHEN to spawn `cavecrew-investigator` (locate code), `cavecrew-builder` (1-2 file edit), or `cavecrew-reviewer` (diff review) instead of doing the work inline or using vanilla `Explore`. Subagent output is caveman-compressed so the tool-result injected back into main context is ~60% smaller — main context lasts lo…
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\cavecrew\SKILL.md
- caveman: Ultra-compressed communication mode. Cuts output tokens 65% (measured) by speaking like caveman while keeping full technical accuracy. Supports intensity levels: lite, full (default), ultra, wenyan-lite, wenyan-full, wenyan-…
  Use when: user says "caveman mode", "talk like caveman", "use caveman", "less tokens", "be brief", or invokes /caveman. Also auto-triggers when token efficiency is requested.
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\caveman\SKILL.md
- caveman-commit: Ultra-compressed commit message generator. Cuts noise from commit messages while preserving intent and reasoning. Conventional Commits format. Subject ≤50 chars, body only when "why" isn't obvious
  Use when: user says "write a commit", "commit message", "generate commit", "/commit", or invokes /caveman-commit. Auto-triggers when staging changes.
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\caveman-commit\SKILL.md
- caveman-compress: Compress natural language memory files (CLAUDE.md, todos, preferences) into caveman format to save input tokens. Preserves all technical substance, code, URLs, and structure. Compressed version overwrites the original file. Human-readable backup saved as FILE.original.md. Trigger: /caveman-compress FILEPATH or "compress memory file"
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\caveman-compress\SKILL.md
- caveman-help: Quick-reference card for all caveman modes, skills, and commands. One-shot display, not a persistent mode. Trigger: /caveman-help, "caveman help", "what caveman commands", "how do I use caveman".
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\caveman-help\SKILL.md
- caveman-review: Ultra-compressed code review comments. Cuts noise from PR feedback while preserving the actionable signal. Each comment is one line: location, problem, fix
  Use when: user says "review this PR", "code review", "review the diff", "/review", or invokes /caveman-review. Auto-triggers when reviewing pull requests.
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\caveman-review\SKILL.md
- caveman-stats: Show real token usage and estimated savings for the current session. Reads directly from the Claude Code session log — no AI estimation
  Use when: /caveman-stats. Output is injected by the mode-tracker hook; the model itself does not compute the numbers.
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\caveman-stats\SKILL.md
- check-work: Check your work with a verification subagent that reviews diffs, runs builds and tests, and evaluates correctness. Read this file for instructions
  Use when: asked to "check work", "verify changes", "self-verify", "/check-work", "/check", "/verify", or "/self-verify".
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\check-work\SKILL.md
- create-skill: Interactively create a new Grok skill (SKILL.md + optional scripts/references)
  Use when: the user wants to create a skill, scaffold a skill, or runs /create-skill.
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\create-skill\SKILL.md
- help: Grok documentation and configuration help
  Use when: users ask about setup, configuration, MCP servers, authentication, skills, slash commands, keyboard shortcuts, or any Grok feature. Also use proactively when you detect a user is having trouble with setup or onboarding.
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\help\SKILL.md
- imagine: How to use the image_gen and image_edit tool calls in Grok Build: when to build a visual with code instead of generating it, prompt-craft, reference-first handling of real people, factual grounding, and asset-consistency. Load this whenever generating or editing an image is on the table, i.e. when an image_gen or image_edit call is being considered or about to be made. Tool-usage-driven, not mer…
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\imagine\SKILL.md
- learn: 把当前会话/任务中可复用的核心知识点——逻辑、品味、思考——精炼成项目根目录的 LEARN.md。
  Use when: the user asks to 总结知识点, 沉淀经验, 记录学到的东西, 写 LEARN.md, 整理心得, distill lessons, save what we learned, or runs /learn.
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\learn\SKILL.md
- ponytail: Forces the laziest solution that actually works, simplest, shortest, most minimal. Channels a senior dev who has seen everything: question whether the task needs to exist at all (YAGNI), reach for the standard library before custom code, n…
  Use when: use whenever the user says "ponytail", "be lazy", "lazy mode", "minimal solution", "yagni", "do less", or "shortest path", or complain…
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\ponytail\SKILL.md
- ponytail-audit: Whole-repo audit for over-engineering. Like ponytail-review, but scans the entire codebase instead of a diff: a ranked list of what to delete, simplify, or replace with stdlib/native equivalents
  Use when: the user says "audit this codebase", "audit for over-engineering", "what can I delete from this repo", "find bloat", "ponytail-audit", or "/ponytail-audit". One-shot report, does not apply fixes.
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\ponytail-audit\SKILL.md
- ponytail-debt: Harvest every `ponytail:` comment in the codebase into a debt ledger, so the deliberate shortcuts and deferrals ponytail leaves behind get tracked instead of rotting into "later means never"
  Use when: the user says "ponytail debt", "/ponytail-debt", "what did ponytail defer", "list the shortcuts", "ponytail ledger", or "what did we mark to do later". One-shot report, changes nothing.
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\ponytail-debt\SKILL.md
- ponytail-gain: Show ponytail's measured impact as a compact scoreboard: less code, less cost, more speed, from the benchmark medians. One-shot display, not a permanent score, and not a per-repo number. Trigger: /ponytail-gain, "ponytail gain", "what does ponytail save", "show ponytail scoreboard".
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\ponytail-gain\SKILL.md
- ponytail-help: Quick-reference card for all ponytail modes, skills, and commands. One-shot display, not a persistent mode. Trigger: /ponytail-help, "ponytail help", "what ponytail commands", "how do I use ponytail".
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\ponytail-help\SKILL.md
- ponytail-review: Code review focused exclusively on over-engineering. Finds what to delete: reinvented standard library, unneeded dependencies, speculative abstractions, dead flexibility. One line per finding: location…
  Use when: the user says "review for over-engineering", "what can we delete", "is this over-engineered", "simplify review", or invokes /ponytail-review. Complements correctness-focused review, this one on…
  Absolute path: C:\Users\19411_4bs7lzt\.grok\skills\ponytail-review\SKILL.md
- akshare: Chinese financial data access using AkShare library. Fetch real-time and historical data for A-shares, Hong Kong stocks, US stocks, futures, funds, and macroec…
  Use when: user requests Chinese market data, stock prices, market analysis, or financial information from Chinese exchanges. Supports stock quotes, historical data, futures market data, fund information, macroeconomic indicators, and real-time m…
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\akshare\SKILL.md
- cloudflare-deploy: Deploy applications and infrastructure to Cloudflare using Workers, Pages, and related platform services
  Use when: the user asks to deploy, host, publish, or set up a project on Cloudflare.
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\cloudflare-deploy\SKILL.md
- copywriting: Write and rewrite marketing copy for landing pages, homepages, and ads. Useful as a copy chief partner during launches.
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\copywriting\SKILL.md
- create-readme: Create a README.md file for the project
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\create-readme\SKILL.md
- defuddle: Extract clean markdown content from web pages using Defuddle CLI, removing clutter and navigation to save tokens. Use instead of WebFetch when the user provides a URL to read or analyze, for online documentation, articles, blog posts, or any web page.
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\defuddle\SKILL.md
- find-skills: Helps users discover and install agent skills when they ask questions like "how do I do X", "find a skill for X", "is there a skill that can...", or express interest in extending capabilities. This skill should be used when the user is looking for functionality that might exist as an installable skill.
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\find-skills\SKILL.md
- frontend-design: Create distinctive, production-grade frontend interfaces with high design quality
  Use when: the user asks to build web components, pages, artifacts, posters, or applications (examples include websites, landing pages, dashboards, React components, HTML/CSS layouts, or when styling/beautifying any UI). Generates creative, polished code and UI design that avoids generic AI aesthetics.
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\frontend-design\SKILL.md
- gh-address-comments: Help address review/issue comments on the open GitHub PR for the current branch using gh CLI; verify gh auth first and prompt the user to authenticate if not logged in.
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\gh-address-comments\SKILL.md
- gh-fix-ci: Inspect GitHub PR checks with gh, pull failing GitHub Actions logs, summarize failure context, then create a fix plan and implement after user approval
  Use when: a user asks to debug or fix failing PR/CI/CD checks on GitHub Actions and wants a plan + code changes; for external checks (e.g., Buildkite), only report the details URL and mark them out of scope.
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\gh-fix-ci\SKILL.md
- humanize-writing: Remove signs of AI-generated…
  Use when: the user mentions
'sounds like AI,' 'too robotic,' 'humanize this,' 'make it sound natural,'
'doesn't sound like a person wrote it.' Detects and fixes AI writing patterns including
inflated significance, formulaic language, surface-level analysis, vague assertions,
hedgi…
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\humanize-writing\SKILL.md
- json-canvas: Create and edit JSON Canvas files (.canvas) with nodes, edges, groups, and connections
  Use when: working with .canvas files, creating visual mind maps, flowcharts, or when the user mentions Canvas files in Obsidian.
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\json-canvas\SKILL.md
- karpathy-guidelines: Behavioral guidelines to reduce common LLM coding mistakes
  Use when: writing, reviewing, or refactoring code to avoid overcomplication, make surgical changes, surface assumptions, and define verifiable success criteria.
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\karpathy-guidelines\SKILL.md
- obsidian-bases: Create and edit Obsidian Bases (.base files) with filters, formulas, and summaries
  Use when: working with .base files, creating database-like views of notes, or when the user mentions Bases, view tables, formulas, or Obsidian.
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\obsidian-bases\SKILL.md
- obsidian-cli: Interact with Obsidian vaults using the Obsidian CLI to read, create, search, and manage notes, tasks, and more. Also supports plugin and theme development with commands to reload plugins, run JavaScript, perform queries…
  Use when: the user asks to interact with their Obsidian vault, manage notes, search vault content, perform vault operations from the command line, or develop and debug Obsidian…
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\obsidian-cli\SKILL.md
- obsidian-markdown: Create and edit Obsidian Flavored Markdown with wikilinks, embeds, callouts, properties, and other syntax
  Use when: working with .md files in Obsidian, or when the user mentions wikilinks, callouts, frontmatter, properties, or Obsidian notes.
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\obsidian-markdown\SKILL.md
- pdf: Use when tasks involve reading, creating, or reviewing PDF files where rendering and layout matter; prefer visual checks by rendering pages (Poppler) and use Python tools such as `reportlab`, `pdfplumber`, and `pypdf` for generation and extraction.
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\pdf\SKILL.md
- pptx: Use this skill any time a .pptx file is involved in any way — as input, output, or both. This includes: creating slide decks, pitch decks, or presentations; reading, parsing, or extracting text from any .pptx file (even if the extracted content will be used elsewhere, like in an email or summary); generating, modifying, or updating existing presentations; combining or splitting slide files; work…
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\pptx\SKILL.md
- pua: 让你的 AI 不敢摆烂。用大厂 PUA 话术穷尽一切方案。触发条件：(1) 任务失败 2+ 次或反复微调同一思路; (2) 即将说'我无法解决'、建议用户手动操作、未验证就归因环境; (3) 被动等待——不搜索、不读源码、只等指示; (4) 用户不满：'try harder'、'stop giving up'、'换个方法'、'为什么还不行'、'你再试试'、'…
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\pua\SKILL.md
- quantitative-research: World-class systematic trading research - backtesting, alpha generation, factor models, statistical arbitrage. Transform hypotheses into edges
  Use when: "backtest, alpha, factor model, statistical arbitrage, systematic trading, mean reversion, momentum strategy, regression analysis, walk forward, " mentioned.
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\quantitative-research\SKILL.md
- runbook-skill: Use before coding, debugging, build, test, deployment, infrastructure, database, repository maintenance, or agent handoff tasks to run `runbook scan`, understand the current project and machine tool environment, choose the right CLI tools, avoid package-manager/build-tool confusion, and res…
  Use when: the user asks what tools are available, which tool should be used, or how an agent should prepare befor…
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\runbook-skill\SKILL.md
- sivtr-memory: Retrieve shared local work memory: terminal activity, AI conversation history, prior decisions, validation evidence, debugging trails, recaps, and handoff context. Use before asking the user to repeat local work context.
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\sivtr-memory\SKILL.md
- stop-slop: Remove AI writing patterns from prose
  Use when: drafting, editing, or reviewing text to eliminate predictable AI tells.
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\stop-slop\SKILL.md
- yeet: Use only when the user explicitly asks to stage, commit, push, and open a GitHub pull request in one flow using the GitHub CLI (`gh`).
  Absolute path: C:\Users\19411_4bs7lzt\.agents\skills\yeet\SKILL.md
