<div align="center">

  <img src="internal/api/static/favicon.svg" width="140" height="140" alt="TalkToThem Logo">

# TalkToThem

**An AI agent that talks to your friends and family for you.**

<a href="#-cloud-deployment"><b>🚀 Deploy Now</b></a> ·
<a href="#getting-started"><b>📖 Getting Started</b></a> ·
<a href="#features"><b>✨ Features</b></a>

</div>

---

TalkToThem learns your conversation style by analyzing your message history, then can hold conversations on your behalf with your contacts. Whether it's catching up with mom, chatting with your girlfriend, or responding to friends—the agent sounds like you.

> **Note:** TalkToThem is designed for **1-on-1 conversations only**. Group chats are not currently supported.

## Features

- **🎯 Learns your voice**: Analyzes your conversation patterns and writing style
- **👁️ Multi-modal**: Sees images and can react to messages with appropriate responses
- **👥 Contact-specific**: Understands different relationships and adjusts accordingly
- **🤝 Hands-free conversations**: Let the agent maintain your social connections
- **🌐 Web UI**: Built-in dashboard with Go templates + HTMX for managing contacts and conversations
- **💾 Persistent storage**: All conversations and styles stored locally

## Supported Messengers

- [ ] Signal
- [ ] WhatsApp

## 🚀 Cloud Deployment

Deploy TalkToThem instantly to cloud:

| Provider     | Deploy                                                                                                                                                         |
| ------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| DigitalOcean | [![Deploy to DO](https://www.deploytodo.com/do-btn-blue.svg)](https://cloud.digitalocean.com/apps/new?repo=https://github.com/julienrbrt/talktothem/tree/main) |

**Quick setup after deployment:**

1. Access your deployed web UI
2. Complete onboarding with your OpenAI API key
3. Link your Signal device (scan QR code)
4. Import contacts and enable the agent

## Getting Started

<details>
<summary><b>Option 1: Docker (Recommended)</b></summary>

```bash
# Clone repository
git clone https://github.com/julienrbrt/talktothem.git
cd talktothem

# Create data directories
mkdir -p data/signal

# Start services
docker compose up -d

# Open web UI and complete onboarding
# http://localhost:8080

# Link Signal device (first time only)
# Open http://localhost:8081/v1/qrcodelink?device_name=talktothem
# Scan QR code with Signal mobile app (Settings > Linked Devices > +)

# View logs
docker compose logs -f talktothem
```

**Data is stored in `./data/`:**

- `data/signal/` - Signal registration keys
- `data/talktothem.db` - SQLite database (config, contacts, messages)

</details>

<details>
<summary><b>Option 2: Install Binary</b></summary>

```bash
go install github.com/julienrbrt/talktothem@latest
```

Requires a running [signal-cli-rest-api](https://github.com/bbernhard/signal-cli-rest-api) instance.

### Usage

```bash
# Start web server
talktothem serve
talktothem serve --addr :3000  # custom port

# Data is stored in ~/.config/talktothem/ by default
# Override with TALKTOTHEM_DATA_PATH environment variable
```

</details>

### Configuration

Configuration is done through the web UI during onboarding. Settings are stored in SQLite database:

- **Signal Phone Number** - Your Signal phone number
- **Signal API URL** - URL of your signal-cli-rest-api instance (default: http://localhost:8081)
- **OpenAI API Key** - Your OpenAI API key
- **Model** - OpenAI model to use (default: gpt-4o)
- **Base URL** - Optional, for OpenAI-compatible APIs

**Environment variable:** `TALKTOTHEM_DATA_PATH` - Path to store database (default: `~/.config/talktothem/`)

## How It Works

1. **Connect** your Signal account via the web UI
2. **Configure** your OpenAI API key in onboarding
3. **Import** contacts from Signal
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
