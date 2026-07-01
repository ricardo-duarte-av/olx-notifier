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
db_path: "./olx.db"
```

`config.yaml` and `*.db` are gitignored.

## Run

```sh
./olx-notifier -config config.yaml
```

## Matrix commands

Send these in the configured room:

| Command | Description |
| --- | --- |
| `!olx add "<query>" <min> <max> <category_id>` | Add a search. Use `-` to skip a filter. |
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
