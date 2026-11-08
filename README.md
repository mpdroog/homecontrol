# homecontrol

Charges the car through a myenergi Zappi in the cheapest day-ahead price
slots before a deadline (e.g. Wednesday and Thursday 07:00, 90%).

Every minute it asks one question — *should the car charge right now?* — and
switches the Zappi between **Fast** and **Stop** accordingly. There is no
schedule state to get stuck: after a restart it simply decides again.

Why not Eco/Eco+? With a home battery that trades on the grid (NextEnergy),
the Zappi sees battery export as "solar surplus": it charges the car with
battery power and keeps stopping and starting. Fast in planned slots avoids that.

## Behaviour

- **Plan**: needed hours = (target − battery %) × capacity ÷ charge power × 1.15.
  The cheapest 15-minute slots before the deadline are chosen (not necessarily
  consecutive). Battery % comes from MySkoda; when unknown (not configured or
  failing) it assumes an empty battery. The Zappi is leading: "complete"
  means Stop.
- **Waits** until prices up to the deadline are known (published ~13:00 the day
  before). Exception: at or below `cheap_price` it always charges.
- **No prices** (fetch failed or tomorrow not published): charges only when
  the deadline is too close to wait.
- **Manual override**: change the Zappi mode in the myenergi app and homecontrol
  keeps its hands off until the car is unplugged.
- **Not charging** while it should: after 10 min it asks the car to start
  (MySkoda), after 20 min it sends a notification.
- **Notifications** (Pushover, or the log): reminder at 20:00 if the car is not
  plugged in before a deadline, session summary on unplug (kWh, average EPEX
  price, kWh that came from the home battery while charging).

## Control flow

`PUSH ▶` marks a Pushover notification (exact title; text in Dutch). Without
`-w` or without `[pushover]` these go to the log only.

```
main
 └─ flags (-c, -v, -w) → run()
     ├─ load config (error → exit 1 → systemd retries in 15 min, max 4× per 2 h)
     ├─ build clients (without -w: dry-run wrappers, PUSH ▶ goes to the log)
     └─ every minute: Tick(now); error → log, keep looping
```

Tick, every minute:

```
1. Zappi status ──(error)──> log, do nothing this minute
     failing ≥ 30 min → PUSH ▶ "Zappi werkt niet"; works again → PUSH ▶ "Zappi werkt weer"
2. Car plugged in?
   ├─ NO → disconnected():
   │    ├─ session ended (≥ 0.1 kWh)?
   │    │    PUSH ▶ "Laadsessie klaar"   kWh, average EPEX price, kWh from home battery
   │    ├─ clear session and override
   │    └─ ≥ 20:00, deadline within 11 h, battery < target, not yet sent?
   │         PUSH ▶ "Auto niet aangesloten"   (once per deadline)
   └─ YES:
      3. new session? → start it, fetch battery level right away
      4. battery level (MySkoda, at most every 15 min; error → log, counts as unknown)
           failing ≥ 24 h → PUSH ▶ "MySkoda werkt niet"; works again → PUSH ▶ "MySkoda werkt weer"
      5. accounting: kWh, EPEX cost, home battery share
      6. override? (mode differs from what we set, after 3 min settle time)
         └─ YES → hands off until unplug
              PUSH ▶ "Slim laden gepauzeerd"   (once per session)
      7. decide(): Stop or Fast
         ├─ Zappi "complete" / battery ≥ target / no charge days → Stop
         ├─ price ≤ cheap_price → Fast
         ├─ no prices up to the deadline (fetch failed / not published):
         │    deadline close → Fast + PUSH ▶ "Laden zonder prijzen" (once per session)
         │    otherwise      → Stop (wait)
         └─ cheapest slots (unknown battery → empty): now planned? → Fast / Stop
      8. apply(): Zappi differs? → SetMode
      9. watchFailure(): we want Fast, car draws < 500 W
         ├─ after 10 min → ask the car to start (MySkoda, once per session)
         └─ after 20 min
              PUSH ▶ "Auto laadt niet"   (once per session)
```

Other errors (Nord Pool, AlphaESS, short Zappi/MySkoda hiccups) and crashes
are only logged (`journalctl -u homecontrol`).

## Configuration

Copy `config.toml.example` to `config.toml`. `[alphaess]`, `[myskoda]` and
`[pushover]` are optional. Unknown keys are an error.

- myenergi API key: myaccount.myenergi.com → Manage Products → hub → Advanced →
  Generate new API key.
- AlphaESS: register the system (serial + check code) on https://open.alphaess.com.

## Run

```sh
go build -o homecontrol .
./homecontrol -v       # dry-run: only logs what it would do (Ctrl+C to stop)
./homecontrol -w       # run for real (Ctrl+C to stop)
```

Raspberry Pi (64-bit OS):

```sh
GOOS=linux GOARCH=arm64 go build -o homecontrol .
scp homecontrol config.toml homecontrol.service pi:/opt/homecontrol/
# on the Pi:
sudo useradd --system homecontrol
sudo cp /opt/homecontrol/homecontrol.service /etc/systemd/system/
sudo systemctl enable --now homecontrol
journalctl -u homecontrol -f
```

## Development

```sh
go test -race ./...
golangci-lint run ./...   # strict config: all linters, see .golangci.yml
```
