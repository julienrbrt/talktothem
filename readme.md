# Talk to Them

An AI agent that talks to your friends and family for you.

TalkToThem learns your conversation style by analyzing your message history, then can hold conversations on your behalf with your contacts. Whether it's catching up with mom, chatting with your girlfriend, or responding to friends—the agent sounds like you.

## Features

- **Learns your voice**: Analyzes your conversation patterns and writing style
- **Multi-modal**: Sees images and can react to messages with appropriate responses
- **Contact-specific**: Understands different relationships and adjusts accordingly
- **Hands-free conversations**: Let the agent maintain your social connections
- **Web UI**: Built-in dashboard with Go templates + HTMX for managing contacts and conversations

## Supported Messengers

| Messenger          | Status    |
| ------------------ | --------- |
| Signal             | Supported |
| WhatsApp           | Planned   |
| Facebook Messenger | Planned   |

## Getting Started

### Option 1: Docker (Recommended)

```bash
# Clone the repository
git clone https://github.com/julienrbrt/talktothem.git
cd talktothem

# Create data directories
mkdir -p data/signal

# Start services
docker compose up -d

# Open the web UI and complete onboarding
# http://localhost:8080

# Link Signal device (first time only)
# Open http://localhost:8081/v1/qrcodelink?device_name=talktothem
# Scan QR code with Signal mobile app (Settings > Linked Devices > +)

# View logs
docker compose logs -f talktothem
```

Data is stored in `./data/`:
- `data/signal/` - Signal registration keys
- `data/talktothem.db` - SQLite database (config, contacts, messages)

### Option 2: Install Binary

```bash
go install github.com/julienrbrt/talktothem@latest
```

Requires a running [signal-cli-rest-api](https://github.com/bbernhard/signal-cli-rest-api) instance.

### Usage

```bash
# Start the web server
talktothem serve
talktothem serve --addr :3000  # custom port

# Data is stored in ~/.config/talktothem/ by default
# Override with TALKTOTHEM_DATA_PATH environment variable
```

### Configuration

Configuration is done through the web UI during onboarding. Settings are stored in the SQLite database:

- **Signal Phone Number** - Your Signal phone number
- **Signal API URL** - URL of your signal-cli-rest-api instance (default: http://localhost:8080)
- **OpenAI API Key** - Your OpenAI API key
- **Model** - OpenAI model to use (default: gpt-4o)
- **Base URL** - Optional, for OpenAI-compatible APIs

Environment variable: `TALKTOTHEM_DATA_PATH` - Path to store database (default: `~/.config/talktothem/`)

## How It Works

1. **Connect** your Signal account via the web UI
2. **Configure** your OpenAI API key in onboarding
3. **Import** contacts from Signal or upload vCard files
4. **Let it learn** from your conversation history (sync button)
5. **Enable** the agent for specific contacts

The agent continuously improves as it observes more of your conversations.

## Architecture

- **Backend**: Go with Chi router
- **Database**: SQLite (via github.com/glebarez/sqlite)
- **Frontend**: Go templates + HTMX + Tailwind CSS
- **AI**: OpenAI API (or compatible)

## Disclaimer

This project is for educational and experimental purposes. Use responsibly and ensure your contacts are aware of AI-assisted communication where appropriate.

## License

[MIT](./license)
