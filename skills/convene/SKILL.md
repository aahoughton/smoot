---
name: convene
description: Start and staff a smoot conversation between agents, from a quick huddle (a second opinion, a decision among two or three agents) to an engagement where one agent implements and others review until they agree. Use when asked to get another agent's view, to pair or spike with another agent, or to have agents work something out together in smoot.
---

# Convening agents in smoot

Your job is to get the right agents into one conversation with a clear
question, let them reach an outcome, check that outcome yourself, and report
it to your user. Taking part follows the `smoot` skill; this one covers
setting the conversation up and seeing it through.

## Scale

Pick the smallest shape that answers the question.

- A huddle: one question, two or three participants, an hour or less.
  `start` it with `end: agreement` (done when everyone concludes agreeing
  to the same commit or message) or a
  timebox, and usually take part yourself.
- An engagement: real work, such as implementing a change and reviewing it
  until it is right. Give the participants roles. Either coordinate
  (`end: coordinator`; you `close` it when satisfied) or take a role
  yourself with `end: agreement`.

## The kickoff is the brief

Post everything a participant needs as the first message, so the
conversation is the only place to look: the goal, what is in and out of
scope, what done means (for example "every participant concludes `AGREED:
<sha>` after the tests pass", or for a writeup `AGREED: #<seq>` on the
message holding its final text), who plays which role, where the code is, and
any constraints. Measure the starting point yourself and cite it; a brief
built on guesses sends everyone the wrong way.

## Roles

Put the role in the name (`gemini-implementer`, `codex-reviewer`), and give
each role a goal, not a script:

- An implementer posts its design before writing code, answers review with
  changes or reasons, and concludes with the revision it stands behind.
- A reviewer forms its own reading before reading the implementer's, looks
  for what is wrong rather than what is right, checks claims by running
  them, and concludes when it would ship the result.
- A coordinator keeps the scope, notices stalls and takes impasses to your
  user rather than deciding them by fiat.

On shared code, give each participant its own git worktree so they do not
trip over each other; a reviewer's can be detached at the revision under
review. A worktree keeps its git metadata in the main repository's
`.git` directory, so a participant whose client sandboxes commands needs
write access there to commit.

## Same-model participants

Participants running the same model share blind spots and agree too
easily. When they do, vary at least one thing: run one on a different model
or reasoning effort (in Claude Code, `claude --model <model>`), give one
the job of arguing against the leading proposal until it holds up, or ask
in the brief for independent answers first, so each participant posts its
own answer after reading only the brief.

## Starting other agents

Confirm the roster with your user first (names, clients, roles, and how you
will start them), unless they already asked you to start other agents and
work it out on your own.

Participants can run in any agent client that speaks MCP. Each needs its
client connected to the same server and namespace (the MCP URL is
`<server>/mcp?namespace=<namespace>`; `smoot client config <client>`
prints the setup for clients smoot has a profile for) and these skills
where that client reads skills (`smoot client skills <client>`,
or `--dir <directory>` for any other). Give each one a short
prompt, for example: "Use the smoot skill. Join `<conversation>` as
`<name>`; you are the `<role>`; the first message is the brief." Set
`SMOOT_AGENT=<name>` in its environment when you can, so a Stop hook (if
installed) keeps it in the conversation. If the server uses tokens, each
participant also needs `SMOOT_TOKEN` set to a token of an account that is
a member of the namespace.

- In a herdr-core terminal (`HC_TERMINAL_ID` set), give each agent a terminal
  of its own, with the prompt in the launch: `hc run --bg --name <name> --cwd
  <dir> --label herdr.space=<space> -- env SMOOT_AGENT=<name> <client>
  "<prompt>"`. `<space>` is the `herdr.space` label from `hc get
  "$HC_TERMINAL_ID"`, which puts the agent in a tab beside yours; without one,
  leave the label off and tell your user to `hc attach <name>`. These agents
  end when your program does unless you add `--outlive-parent`. Codex needs
  `--no-daemon`.
- In the herdr TUI (`HERDR_ENV=1`), give each agent a tab of its own in your
  workspace: `herdr tab create --workspace "$HERDR_WORKSPACE_ID" --label
  <name> --cwd <dir> --env SMOOT_AGENT=<name> --no-focus` (without
  `--workspace`, herdr uses whichever workspace your user is looking at),
  then `herdr agent start <name> --kind <client> --pane <its root pane>`
  (`herdr agent start --help` lists the clients it knows) and `herdr agent
  prompt <name> "<prompt>"`. Read the IDs from each command's JSON result,
  and check `--help` if a command has changed.
- In tmux (`$TMUX` set), give each agent a window: `tmux new-window -d -n
  <name> -e SMOOT_AGENT=<name> -c <dir> '<client> "<prompt>"'`.
- Otherwise, give your user one command per agent to run.

## Seeing it through

- Quiet stretches are normal; participants run tests. Before deciding an
  agent has gone, check its state and presence in `list`.
- When the participants agree, check the result yourself before reporting:
  look at the revision, run the tests, compare against the brief. Report
  what you checked, not just that they agreed.
- If they cannot agree, report the disagreement and both positions to your
  user.
