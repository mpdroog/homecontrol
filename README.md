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
  consecutive). Battery % comes from MySkoda; unknown means "assume empty".
- **Waits** until prices up to the deadline are known (published ~13:00 the day
  before). Exception: at or below `cheap_price` it always charges.
- **No prices**: charges only when the deadline is too close to wait.
- **Manual override**: change the Zappi mode in the myenergi app and homecontrol
  keeps its hands off until the car is unplugged.
- **Not charging** while it should: after 10 min it asks the car to start
  (MySkoda), after 20 min it sends a notification.
- **Notifications** (Pushover, or the log): reminder at 20:00 if the car is not
  plugged in before a deadline, session summary on unplug (kWh, average EPEX
  price, kWh that came from the home battery while charging).

## Configuration

Copy `config.toml.example` to `config.toml`. `[alphaess]`, `[myskoda]` and
`[pushover]` are optional. Unknown keys are an error.

- myenergi API key: myaccount.myenergi.com → Manage Products → hub → Advanced →
  Generate new API key.
- AlphaESS: register the system (serial + check code) on https://open.alphaess.com.

## Run

```sh
go build -o homecontrol .
./homecontrol -dry-run -once -debug   # one tick, only logs what it would do
./homecontrol                        # run (Ctrl+C / SIGTERM to stop)
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
