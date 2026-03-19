# Talk to Them

An AI agent that talks to your friends and family for you.

TalkToThem learns your conversation style by analyzing your message history, then can hold conversations on your behalf with your contacts. Whether it's catching up with mom, chatting with your girlfriend, or responding to friends—the agent sounds like you.

## Features

- **Learns your voice**: Analyzes your conversation patterns and writing style
- **Multi-modal**: Sees images and can react to messages with appropriate responses
- **Contact-specific**: Understands different relationships and adjusts accordingly
- **Hands-free conversations**: Let the agent maintain your social connections

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

# Create config file (or use: go run . config init)
cat > config.yaml << 'EOF'
signal:
  phone_number: "+1234567890"
  api_url: "http://localhost:8080"

agent:
  api_key: "sk-..."  # your OpenAI API key
  model: "gpt-4o"

contact:
  data_path: ""
EOF

# Create data directories
mkdir -p data/signal data/contacts

# Start services
docker compose up -d

# Link Signal device (first time only)
# Open http://localhost:8080/v1/qrcodelink?device_name=talktothem
# Scan QR code with Signal mobile app (Settings > Linked Devices > +)

# View logs
docker compose logs -f talktothem
```

Data is stored in `./data/`:
- `data/signal/` - Signal registration keys
- `data/contacts/` - Contact settings and learned styles

### Option 2: Install Binary

```bash
go install github.com/julienrbrt/talktothem@latest
```

Requires a running [signal-cli-rest-api](https://github.com/bbernhard/signal-cli-rest-api) instance.

### Configuration

Create `~/.config/talktothem/config.yaml`:

```yaml
signal:
  phone_number: "+1234567890"
  api_url: "http://localhost:8080"  # signal-cli-rest-api endpoint

agent:
  api_key: "sk-..."  # OpenAI API key
  model: "gpt-4o"
  base_url: ""  # optional, for OpenAI-compatible APIs

contact:
  data_path: ""  # optional, defaults to ~/.config/talktothem/contacts
```

Environment variables are also supported: `TALKTOTHEM_SIGNAL_API_URL`, `TALKTOTHEM_AGENT_API_KEY`, etc.

### Usage

```bash
# Initialize config file
talktothem config init
talktothem config init -o ./config.yaml  # custom path
talktothem config init -f                # force overwrite

# Interactive mode - select contact from list
talktothem run

# Specify contact directly
talktothem run "+1234567890"
talktothem run "Mom"

# Options
talktothem run --dry-run              # Preview without sending
talktothem run --initiate             # Start conversation if no response needed
talktothem run --response-window 48h  # Response window (default: 24h)
```

## How It Works

1. **Connect** your Signal account
2. **Select** a contact to talk to
3. **Let it learn** from your conversation history
4. **Enable** the agent to respond on your behalf

The agent continuously improves as it observes more of your conversations.

## Disclaimer

This project is for educational and experimental purposes. Use responsibly and ensure your contacts are aware of AI-assisted communication where appropriate.

## License

[MIT](./license)
