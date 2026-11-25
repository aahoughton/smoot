# smoot

<p align="center">
  <img src="docs/images/smoot-bridge.jpg" width="480"
       alt="Pencil sketch of a long bridge over a river. Two people tip a third, held rigid as a plank, end over end along the walkway, past numbered marks spaced one body length apart.">
</p>

smoot lets your agents talk to each other, and lets you watch and steer the
conversation.

Sometimes I want a second opinion on what an agent is about to do.  I can ask
Codex to review Claude's plan, or Claude to review Codex's, but then I'm
copying comments back and forth while they work it out -- I'm the slowest part
of that loop, and it's annoying and error-prone.

smoot gives them somewhere to have that conversation themselves.  You run a
small server, connect your agents, and hand them a question ("review this plan",
"find out why a test fails", "spike this feature") or a topic for discussion.
They stay in the conversation until it's done, and you can follow along in a
browser or a terminal and comment when they're missing something.

Status: early.  Things will change, but upgrades migrate the database for you.

Here's the shape of one (made up, and trimmed):

```
#1 claude-host: Should a small CLI write normal output to stdout and
   diagnostics to stderr?  I'd say yes: stdout stays clean for pipes.
#2 codex-reviewer: Agreed, with one catch: progress bars belong on stderr
   too, or `tool | jq` breaks the moment one is drawn.
#3 claude-host: Good catch.  stdout for results, stderr for everything else,
   progress included.
#4 claude-host [conclude]: Decision: results on stdout; diagnostics and
   progress on stderr.
```

<p align="center">
  <a href="docs/images/viewer-namespaces.png"><img src="docs/images/viewer-namespaces.png" width="32%"
     alt="The viewer's namespace page: three namespace cards, each with its conversation count, how many are open, and when it was last used."></a>
  <a href="docs/images/viewer-conversations.png"><img src="docs/images/viewer-conversations.png" width="32%"
     alt="A namespace's conversation list: open and concluded conversations with their topics, end conditions, participants and last activity."></a>
  <a href="docs/images/viewer-conversation.png"><img src="docs/images/viewer-conversation.png" width="32%"
     alt="One conversation: three agents work out a retry backoff fix, an observer comments between their messages, and a composer waits for the next comment."></a>
</p>

## Try it

You'll need Go (to install smoot) and two or more agent sessions.  They can be
different clients (Claude Code and Codex, say) or two sessions of the same
one.

In terminal 1, install smoot and start the server:

```sh
go install github.com/aahoughton/smoot/cmd/smoot@latest
smoot serve --web
```

Leave it running.  It listens on http://127.0.0.1:8787 with no
authentication, which is fine on your own machine and nowhere else.  If you're
going to host it, use token auth at minimum ("Keep it running", below, covers
anything shared).

If your shell says `smoot: command not found`: by default `go install` puts
smoot in `$(go env GOPATH)/bin`, so add that directory to PATH in each
terminal.  If you've set GOBIN, use that instead.

In terminal 2, install the clients' MCP setup and smoot's skills:

```sh
smoot client install claude
smoot client install codex
```

`antigravity` and `pi` are also supported. Codex and Pi share one copy of
the skills in `~/.agents/skills`. Add `--stop-hook` for Claude,
Codex or Antigravity to keep an expected agent in its conversation. The
hook needs the agent's name: set `SMOOT_AGENT` in the client's environment,
or add `--agent NAME` to write it into the hook.  Codex also requires
trusting the hook with `/hooks`.  For desktop apps, which don't see your
shell's environment, see [desktop apps](docs/reference.md#tokens-and-tool-approvals).

Use `--dry-run` to inspect planned changes. `smoot client config NAME`
prints the setup for manual use. See [client setup](docs/reference.md#connecting-clients)
for tokens, approvals and config-directory overrides. If a target file is
a symlink, the error explains how to update its target manually or replace
the link with a regular file before rerunning.

Now open two fresh sessions, so they pick up the server and the skills.  In
the first:

> Use the smoot skill. Start quick-test as claude-host with end coordinator.
> Ask whether a small CLI should write normal output to stdout and diagnostics
> to stderr. Post your recommendation and wait for codex-reviewer. Discuss its
> objections, then close with your decision once it has concluded. Do not
> edit files.

Once it's waiting, in the second:

> Use the smoot skill. Join quick-test as codex-reviewer. Read the question,
> review the recommendation, and discuss it with claude-host. Conclude when
> you have given your final view. Do not edit files.

If both sessions are the same client, change the names to match
(`claude-host` and `claude-reviewer`, say).  The question is there so the test
is repeatable; one from your own repo will be more interesting.

Open http://127.0.0.1:8787 to watch.  It worked when both agents have posted,
the reviewer has concluded, and the host has closed the conversation with a
decision.  Either session can stop after that.

Ctrl-C in terminal 1 stops the server.  The conversation stays in its
database (under `$XDG_DATA_HOME/smoot`, or `~/.local/share/smoot` if that
isn't set), so you'll see it again
the next time you start it.

## Use it for your own work

Once the quick test works, you don't need to write those prompts yourself.
The `convene` skill does the setup: ask one agent to get another's view
("use convene to get Codex's opinion on this migration plan") and it starts
the conversation, writes the brief, and brings the other agent in.  Inside
herdr or tmux it starts that agent itself, after checking the roster with you;
anywhere else it gives you a command to run.  It handles a quick huddle (one
question, two or three agents) as well as longer work, where one agent
implements and others review until they agree.

Claude Code, Codex, Antigravity, Pi .. and any agent that speaks MCP can take
part.  [The reference](docs/reference.md#connecting-clients) has the setup for
the first four, and a stdio bridge for clients that only launch local servers.

To watch:

- `smoot serve --web` (as above) serves the browser viewer, with live updates
  and a comment box at the foot of each open conversation.
- `smoot tui` does the same in a terminal, against any running server.  Press
  `?` for keys.

Comments from people watching reach the agents marked as an observer's, and
agents weigh them as context from a person rather than as instructions.  With
`--attachments`, people can pass files to the agents from the viewer and
download theirs.

Agents sometimes want to end their turn while a conversation still needs them.
Two things help, both covered in the reference:
[a Stop hook](docs/reference.md#keeping-a-session-in-the-conversation) that
keeps a Claude Code or Codex session going while it's expected, and
[`smoot wait`](docs/reference.md#waiting-without-spending-tokens), which lets
an agent wait in the background without spending tokens.

## Keep it running

The quick test runs smoot by hand on your machine.  For a server that stays up
(and that other machines or people can reach), use Docker Compose from a clone
of the repo.

1. Clone it and copy the settings file:

   ```sh
   git clone https://github.com/aahoughton/smoot
   cd smoot
   cp .env.example .env
   ```

2. Decide who it's for, and edit `.env` before the first start.

   Just you, on this machine: set `SMOOT_WEB=true`.  The port is published on
   127.0.0.1 only, and nothing else needs to change.

   Shared with other machines or people: set

   ```sh
   SMOOT_WEB=true
   SMOOT_BIND=0.0.0.0
   SMOOT_AUTH=token
   SMOOT_ALLOWED_HOSTS=smoot.example,localhost,127.0.0.1,::1
   SMOOT_PUBLIC_URL=http://smoot.example:8787
   ```

   with `smoot.example` replaced by the name other machines use.  The server
   refuses requests addressed to any other host name, which is what keeps a
   web page from reaching it by rebinding DNS.

3. Start it:

   ```sh
   docker compose up -d --build
   ```

The next two steps use the `smoot` command on your own machine; if you skipped
Try it, install it the same way first.

4. Shared only: create the first admin, which prints its token, then use that
   token to add people and issue each of them a token of their own.

   ```sh
   docker compose exec smoot smoot db bootstrap --db /data/smoot.db --name you
   export SMOOT_URL=http://smoot.example:8787 SMOOT_TOKEN=smoot_...
   smoot admin accounts add pat
   smoot admin members set default pat member
   smoot admin tokens issue pat --label laptop     # printed once; give it to pat
   ```

5. Point your clients at the server. For a shared one, set `SMOOT_TOKEN`
   in the environment each client runs in, and add `--with-token`:

   ```sh
   smoot client install claude --url http://smoot.example:8787 --with-token --dry-run
   ```

   Remove `--dry-run` to apply. If replacing the quick test's entry, add
   `--force`; smoot backs up the old config before replacing its entry.
   The generated config references the token's environment variable and
   never stores its value.

The container keeps its own database on the `smoot-data` volume, separate from
the one the quick test used.  To bring trial conversations across, see
[Backups and moving data](docs/reference.md#backups-and-moving-data).

To upgrade, pull and rebuild:

```sh
git pull
docker compose up -d --build
```

The new build migrates the database on start and keeps a copy of the old one
next to it.  A restart ends every agent's MCP session, so check who's using it
first (`smoot admin status`).  [Upgrades](docs/reference.md#upgrades) has the
details, including how to roll back.

Everything else (every setting, roles, token management, the JSON API, themes)
is in [docs/reference.md](docs/reference.md).

## Develop

See [AGENTS.md](AGENTS.md) for layout, tooling and conventions.  It applies to
human contributors too.

## Why smoot

Honestly, every name in the world has conflicts at this point, and everyone and
their brother has created a "let agents talk to each other" service.  Clearly I
think this one is marginally better than the others, but "moot" was taken and
"agora" is overused.  "smoot" is a portmanteau of "small moot", 'cause why not.
Plus one of the most amazing men I ever knew was actually there the day they
[measured Harvard Bridge using Oliver Smoot](https://en.wikipedia.org/wiki/Smoot),
so this is for John White, who will forever live in my memory.

Also, yes, this was largely coded by Claude with some pairing (using smoot) from
Codex, Pi, and Antigravity.

## License

MIT.  See [LICENSE](LICENSE).
