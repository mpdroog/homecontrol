# homecontrol

CLI tool for monitoring and controlling home energy devices.

## Supported Devices

- **MySkoda** - Electric vehicle status and charging control
- **AlphaESS** - Home battery storage monitoring
- **NordPool** - Energy price monitoring
- **myenergi Zappi** - EV charger status and control

## Installation

```bash
go build
```

## Configuration

Copy `config.toml.example` to `config.toml` and fill in your credentials:

```toml
[myskoda]
username = "your-email@example.com"
password = "your-password"

[alphaess]
appid = "your-app-id"
appsecret = "your-app-secret"
sn = "ALD007"

[myenergi]
hubserial = "12345678"
password = "your-api-key"
```

### Getting Credentials

**MySkoda**: Use your MySkoda app login credentials.

**AlphaESS**: Register at https://open.alphaess.com/ to get your App ID and App Secret.

**myenergi**:
- Hub serial: Found in the myenergi app under your hub settings
- Password: Generate an API key in the myenergi app

## Usage

```
homecontrol [options] [command]
```

### Options

| Option | Description |
|--------|-------------|
| `-config` | Path to config file (default: `config.toml`) |
| `-debug` | Enable debug output |

### Commands

#### MySkoda (Electric Vehicle)

| Command | Description |
|---------|-------------|
| `status` | Show battery/charging status (default command) |
| `start [VIN]` | Start charging (uses first vehicle if VIN not specified) |
| `stop [VIN]` | Stop charging (uses first vehicle if VIN not specified) |
| `limit [VIN] PCT` | Set charge limit to PCT percent |

#### AlphaESS (Home Battery)

| Command | Description |
|---------|-------------|
| `battery` | Show AlphaESS home battery status |

#### NordPool (Energy Prices)

| Command | Description |
|---------|-------------|
| `prices` | Show hourly energy prices (today & tomorrow) |

#### myenergi Zappi (EV Charger)

| Command | Description |
|---------|-------------|
| `zappi` | Show Zappi EV charger status |
| `zappi-start` | Start Zappi charging (Fast mode) |
| `zappi-stop` | Stop Zappi charging |
| `zappi-eco` | Set Zappi to Eco mode |
| `zappi-eco+` | Set Zappi to Eco+ mode |
| `zappi-boost KWH` | Boost charge for KWH kilowatt-hours |

## Examples

```bash
# Show vehicle status
./homecontrol status

# Show energy prices
./homecontrol prices

# Show home battery status
./homecontrol battery

# Show Zappi status
./homecontrol zappi

# Start Zappi charging
./homecontrol zappi-start

# Stop Zappi charging
./homecontrol zappi-stop

# Set Zappi to Eco+ mode
./homecontrol zappi-eco+

# Boost charge 15 kWh
./homecontrol zappi-boost 15

# Use custom config file
./homecontrol -config /path/to/config.toml status

# Enable debug output
./homecontrol -debug zappi
```

## License

MIT
