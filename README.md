# memory-gardener

memory-gardener keeps a [Wayminder](https://github.com/kilo666mj/wayminder)
memory store honest. Agents write memories like "verified 2026-09-27 against
`internal/service/service.go` at `cff62bf`" or "no remote yet", and nothing
tells them when the code moves on. Once a day the gardener:

1. **Applies answered reviews.** For each review a person has answered, it
   supersedes or forgets the memory as they chose and closes the task.
2. **Checks every memory mechanically.** It extracts the repositories, file
   paths, commits and host names a memory cites and asks git and DNS whether
   they still hold: is the path gone, has it changed since the cited commit or
   verification date, is the repository retired, does the host still resolve,
   and has a "not yet" claim aged while the repository kept moving?
3. **Judges what looks doubtful.** A local OpenAI-compatible model reads the
   memory alongside the commits and diffs and returns `still_true`,
   `needs_update`, `obsolete` or `unsure`, with a confidence. An update is a
   list of exact find-and-replace edits, not a rewrite: each edit must quote
   text that occurs exactly once in the memory, or the verdict becomes
   `unsure`. Small models cannot then lose the parts they did not touch.
4. **Asks a person.** Each proposed change becomes one Taskboard task with a
   blocking question: *Apply replacement*, *Forget memory*, *Keep unchanged*,
   or your own wording. It writes nothing to Wayminder without an answer.
5. **Reports** a `memory_gardener` check to Fleetglass.

The gardener remembers what it has asked and what was decided. A kept memory
comes back only when new commits touch the files it cites. A memory judged
current is not judged again until something new happens. If the model is
down, flagged memories wait for the next run.

## Build and test

```sh
go build ./...
go test -race ./...
go vet ./...
```

## Run

Every setting is an environment variable; the policy is a JSON file
([docs/policy.md](docs/policy.md)).

| Variable | Purpose |
| --- | --- |
| `MEMORY_GARDENER_WAYMINDER_URL`, `_TOKEN` | Wayminder MCP endpoint and a registered client token (required) |
| `MEMORY_GARDENER_TASKBOARD_URL`, `_TOKEN` | Taskboard base URL (its `/mcp` endpoint is used; agent tokens are not accepted on the REST API) and an agent credential (required unless `-dry-run`) |
| `MEMORY_GARDENER_JUDGE_URL`, `_MODEL`, `_API_KEY`, `_TIMEOUT` | OpenAI-compatible endpoint (default `http://127.0.0.1:8080/v1`) |
| `MEMORY_GARDENER_FLEETGLASS_URL`, `_TOKEN`, `_HOST` | Optional Fleetglass ingest |
| `MEMORY_GARDENER_POLICY` | Policy file (default `/etc/memory-gardener/policy.json`) |
| `MEMORY_GARDENER_DATA_DIR` | Repository mirrors and `state.json` (default `/var/lib/memory-gardener`) |

```sh
memory-gardener -no-judge     # mechanical findings only; writes nothing
memory-gardener -dry-run      # also asks the model; writes nothing
memory-gardener               # the real run
memory-gardener -json         # machine-readable report
```

### Judge model

Any OpenAI-compatible server with JSON-schema output works. With Ollama, make
sure the context window fits the evidence (up to about 8,000 tokens): Ollama's
OpenAI endpoint cannot set it per request, and its default silently truncates.
An alias shares the weights:

```sh
printf 'FROM qwen3:8b\nPARAMETER num_ctx 24576\nPARAMETER temperature 0\n' > Modelfile
ollama create qwen3:8b-gardener -f Modelfile
MEMORY_GARDENER_JUDGE_URL=http://127.0.0.1:11434/v1 MEMORY_GARDENER_JUDGE_MODEL=qwen3:8b-gardener memory-gardener -dry-run
```

`qwen3:8b` takes about a minute per memory on a 16 GB consumer GPU. Its
confidence ratings are optimistic, which is why only `high` reaches people by
default and every change still needs an answer.

### Wayminder client

Give the gardener its own Wayminder client: Wayminder rate-limits per client,
and a shared token would spend your interactive agents' budget. The gardener
paces itself at about 100 calls a minute and waits out `429` responses.

In Taskboard, list the gardener's principal in `TASKBOARD_ROUTINE_PRODUCERS`
so its questions arrive in the daily review instead of one notification each,
and set `task.answerers` in the policy to the people allowed to decide.

## Deploy

The playbook installs the binary, a policy file, and a oneshot service with a
daily timer. By default it creates a system user and hardened system units.
Set `memory_gardener_scope: user` to install systemd user units for the
connecting account instead: no root is needed, and git reuses that account's
SSH access to private repositories (enable lingering so the timer fires while
logged out). Keep real endpoints, tokens and the repository list in a private
inventory.

```sh
cd ansible
cp inventory.example inventory
cp host_vars/example.yml.example host_vars/<host>.yml
ansible-playbook playbook.yml
```

It finishes by checking the installed version and making one `-no-judge` run
as the service user.
