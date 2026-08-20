# olx-notifier

A long-lived Go daemon that watches [OLX.pt](https://www.olx.pt/) searches and
posts **new listings** and **price changes** into a Matrix room. Searches are
managed at runtime with `!olx` commands in that room.

## How it works

- Polls the public OLX offers API (`/api/v1/offers/`, no auth) on an interval,
  paginating and deduplicating results per search.
- Stores every ad it has seen per search in SQLite. On each poll:
  - an ad not seen before → **new**;
  - a seen ad whose price changed → **price change**;
  - otherwise → ignored.
- A newly added search is **seeded silently** on its first poll (all current ads
  stored, no messages) so adding a search never floods the room.
- Runs a Matrix client (mautrix-go) in the same process for commands + output.

Each notification sends the ad's **main photo** with a caption (title, price,
location, truncated description) and a link. Any **additional photos** are posted
as replies **in a thread** under that message. Ads with no photo fall back to a
text message. Images are re-uploaded to your homeserver's media repo so they
render inline in any client.

## Build

```sh
go build -o olx-notifier .
```

## Configure

Copy `config.example.yaml` to `config.yaml` and fill in your Matrix homeserver,
user id, access token and room id:

```yaml
matrix:
  homeserver: "https://matrix.org"
  user_id: "@olxbot:matrix.org"
  access_token: "syt_..."
  room_id: "!abc:matrix.org"
poll:
  interval_seconds: 300   # minimum 30
  max_pages: 10           # pagination cap per search (50 ads/page)
  jitter_percent: 10      # spread each tick by ±this much (0-50, 0 disables)
db_path: "./olx.db"
```

`config.yaml` and `*.db` are gitignored.

Each poll waits `interval_seconds` ± `jitter_percent`, so the daemon does not
hit OLX on a perfectly regular beat. If every search fails in a cycle — an
outage, or OLX blocking this host — the bot posts a warning to the room and
posts again once polling recovers, so the silence is never mistaken for "no
new ads".

## Run

```sh
./olx-notifier -config config.yaml
```

## Docker

Each push builds a multi-arch image (`linux/amd64`, `linux/arm64`) and publishes
it to GHCR as `ghcr.io/ricardo-duarte-av/olx-notifier`, tagged with the branch
name, the commit sha, and `latest` on the default branch.

The container keeps all state in `/data`, which is meant to be a bind mount from
the host: you put `config.yaml` there, and the daemon writes `olx.db` next to it.
Set `db_path` accordingly:

```yaml
db_path: "/data/olx.db"
```

```sh
mkdir -p data
cp config.example.yaml data/config.yaml   # then fill it in, set db_path: /data/olx.db
docker run -d --name olx-notifier --restart unless-stopped \
  -v "$PWD/data:/data" ghcr.io/ricardo-duarte-av/olx-notifier:latest
```

Or with the bundled `docker-compose.yaml`:

```sh
docker compose up -d
docker compose logs -f
```

The image runs as uid `10001`, so the host directory must be writable by it
(`chown -R 10001:10001 data`) — otherwise uncomment `user:` in the compose file
and run as your own uid. `data/` is gitignored.

To build the image locally:

```sh
docker build -t olx-notifier .
```

## Matrix commands

Send these in the configured room:

| Command | Description |
| --- | --- |
| `!olx add "<query>" <min> <max> <category_id>` | Add a search. Use `-` to skip a filter. |
| `!olx categories [term\|id]` | No arg lists the top-level sections; a number drills into that category's children; text searches by name. Use the id as `<category_id>`. |
| `!olx list` | List searches with their `#index`, state, filters and ad counts. |
| `!olx disable <index>` | Stop searching an entry (kept in the DB). |
| `!olx enable <index>` | Resume a disabled entry (silently re-baselines on next poll). |
| `!olx delete <index>` | Permanently delete an entry and its stored results. |
| `!olx` / `!olx help` | Show the command list. |

The `<index>` is the `#N` shown by `!olx list`. Disabling keeps the entry and
its stored ads but skips it while polling; enabling resets its baseline so ads
posted while it was disabled are absorbed silently rather than firing a burst of
notifications.

### Multi-user

Each search records the Matrix user who added it. By default users only see and
manage their own searches:

- `!olx list` shows only your searches; `disable`/`enable`/`delete` only work on
  searches you own.
- **Room moderators** (power level ≥ 50) see everyone's searches in `!olx list`
  (with the owner shown) and can `disable`/`enable`/`delete` any of them.

When a new listing or price change is found, the notification **pings the search
owner** (via an `m.mentions` mention at the end of the caption), so the right
person is notified.

Example — iPhones between 100 and 200 € in the iPhone category (id 5407):

```
!olx add "iphone" 100 200 5407
```

The four filters map to OLX API parameters `query`, `filter_float_price:from`,
`filter_float_price:to` and `category_id`. Category ids are OLX's numeric
category ids (e.g. iPhone = 5407); a `!olx categories` lookup helper is a
planned follow-up.

## Tests

```sh
go test -short ./...                          # offline unit tests
go test ./internal/olx -run TestLiveSearch -v # hits the live OLX API
```
