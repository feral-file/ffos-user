# FFOS-USER - Component and User Data Repository

## Overall State

[![Code Coverage](https://img.shields.io/codecov/c/github/feral-file/ffos-user/develop?label=code%20coverage&logo=codecov)](https://codecov.io/gh/feral-file/ffos-user)

| Component              | Build Status                                                                                                                                                                                                                           | Lint Status                                                                                                                                                                                                                          | Code Coverage                                                                                                                                                                       |
| ---------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **feral-controld**     | [![Build](https://img.shields.io/github/actions/workflow/status/feral-file/ffos-user/test-controld.yaml?branch=develop&label=build&logo=github)](https://github.com/feral-file/ffos-user/actions/workflows/test-controld.yaml)         | [![Lint](https://img.shields.io/github/actions/workflow/status/feral-file/ffos-user/lint-controld.yaml?branch=develop&label=lint&logo=github)](https://github.com/feral-file/ffos-user/actions/workflows/lint-controld.yaml)         | [![Coverage](https://img.shields.io/codecov/c/github/feral-file/ffos-user/develop?flag=feral-controld&label=coverage&logo=codecov)](https://codecov.io/gh/feral-file/ffos-user)     |
| **feral-setupd**       | [![Build](https://img.shields.io/github/actions/workflow/status/feral-file/ffos-user/test-setupd.yaml?branch=develop&label=build&logo=github)](https://github.com/feral-file/ffos-user/actions/workflows/test-setupd.yaml)             | [![Lint](https://img.shields.io/github/actions/workflow/status/feral-file/ffos-user/lint-setupd.yaml?branch=develop&label=lint&logo=github)](https://github.com/feral-file/ffos-user/actions/workflows/lint-setupd.yaml)             | [![Coverage](https://img.shields.io/codecov/c/github/feral-file/ffos-user/develop?flag=feral-setupd&label=coverage&logo=codecov)](https://codecov.io/gh/feral-file/ffos-user)       |
| **feral-sys-monitord** | [![Build](https://img.shields.io/github/actions/workflow/status/feral-file/ffos-user/test-sys-monitord.yaml?branch=develop&label=build&logo=github)](https://github.com/feral-file/ffos-user/actions/workflows/test-sys-monitord.yaml) | [![Lint](https://img.shields.io/github/actions/workflow/status/feral-file/ffos-user/lint-sys-monitord.yaml?branch=develop&label=lint&logo=github)](https://github.com/feral-file/ffos-user/actions/workflows/lint-sys-monitord.yaml) | [![Coverage](https://img.shields.io/codecov/c/github/feral-file/ffos-user/develop?flag=feral-sys-monitord&label=coverage&logo=codecov)](https://codecov.io/gh/feral-file/ffos-user) |
| **feral-watchdog**     | [![Build](https://img.shields.io/github/actions/workflow/status/feral-file/ffos-user/test-watchdog.yaml?branch=develop&label=build&logo=github)](https://github.com/feral-file/ffos-user/actions/workflows/test-watchdog.yaml)         | [![Lint](https://img.shields.io/github/actions/workflow/status/feral-file/ffos-user/lint-watchdog.yaml?branch=develop&label=lint&logo=github)](https://github.com/feral-file/ffos-user/actions/workflows/lint-watchdog.yaml)         | [![Coverage](https://img.shields.io/codecov/c/github/feral-file/ffos-user/develop?flag=feral-watchdog&label=coverage&logo=codecov)](https://codecov.io/gh/feral-file/ffos-user)     |

---

## Architecture Overview

FFOS-USER provides components and user data to the FFOS build system. Build orchestration lives in the FFOS repo, while this repo focuses on component source and user data. CI workflows here run tests and lint only.

```
┌───────────────────────────────────────────────────────────┐
│                    ffos-user Repository                   │
│                                                           │
│  ┌──────────────────┐    ┌─────────────────┐              │
│  │   components/    │    │     users/      │              │
│  │                  │    │                 │              │
│  │ ┌──────────────┐ │    │ ┌─────────────┐ │              │
│  │ │feral-controld│ │    │ │  feralfile  │ │              │
│  │ │feral-setupd  │ │    │ │  soaktest   │ │              │
│  │ │feral-sys-    │ │    │ │             │ │              │
│  │ │  monitord    │ │    │ │ Configs     │ │              │
│  │ │feral-app-    │ │    │ │ Scripts     │ │              │
│  │ │  monitord    │ │    │ │ Data        │ │              │
│  │ │feral-watchdog│ │    │ └─────────────┘ │              │
│  │ │launcher-ui   │ │    │                 │              │
│  │ │player-wrapper│ │    │                 │              │
│  │ │     -ui      │ │    │                 │              │
│  │ └──────────────┘ │    │                 │              │
│  └──────────────────┘    └─────────────────┘              │
└───────────────────────────────────────────────────────────┘
                              │
                              ▼
                    ┌─────────────────┐
                    │   ffos Build    │
                    │   Repository    │
                    └─────────────────┘
```

## Repository Structure

```
ffos-user/
├── components/                    # Service components
│   ├── feral-controld/           # Connection daemon
│   │   ├── main.go
│   │   ├── go.mod
│   │   └── ...
│   ├── feral-setupd/             # Setup daemon (Rust)
│   │   ├── src/
│   │   ├── Cargo.toml
│   │   └── ...
│   ├── feral-sys-monitord/       # System monitoring
│   ├── feral-watchdog/           # System watchdog
│   ├── launcher-ui/              # Launcher UI components
│   └── player-wrapper-ui/        # Player wrapper UI
└── users/                        # User data and configurations
    ├── feralfile/                # feralfile user data
    │   ├── scripts/              # User scripts
    │   │   ├── start-kiosk.sh
    │   │   ├── feral-updater.sh
    │   │   └── ...
    │   ├── .config/              # User configurations
    │   │   │   ├── systemd/user/
    │   │   │   │   ├── feral-sys-monitord.service
    │   │   │   │   ├── feral-setupd.service
    │   │   │   │   ├── chromium-kiosk.service
    │   │   │   │   └── ...
    │   │   ├── controld.json
    │   │   └── watchdog.json
    │   ├── .bash_profile         # Shell configuration
    │   └── ...
    └── soaktest/                 # soaktest user data
        ├── scripts/              # Test scripts
        ├── logs/                 # Test logs
        ├── files/                # Test files
        └── ...
```

## Component Architecture

### Service Components Layer

#### 1. Connection Layer (`feral-controld`)

- **Purpose**: Manages device connectivity and communication
- **Language**: Go
- **Dependencies**: WebSocket, HTTP, CDP
- **Build Output**: `feral-controld-{version}-x86_64.pkg.tar.zst`

#### 2. Setup Layer (`feral-setupd`)

- **Purpose**: Handles device initialization and configuration
- **Language**: Rust
- **Dependencies**: Bluetooth, WiFi, System APIs
- **Build Output**: `feral-setupd-{version}-x86_64.pkg.tar.zst`

#### 3. Monitoring Layer

- **feral-sys-monitord**: System resource monitoring
- **feral-watchdog**: System watchdog and recovery

#### 4. UI Layer

- **launcher-ui**: Main launcher interface
- **player-wrapper-ui**: Media player wrapper interface

### User Data Layer

#### feralfile User

```
users/feralfile/
├── scripts/                      # System scripts
│   ├── start-kiosk.sh           # Kiosk mode startup
│   ├── feral-updater.sh         # System update
│   ├── log-rotation.sh          # Log management
│   └── ...
├── .config/                      # Application configs
│   ├── systemd/user/             # Systemd services
│   │   ├── feral-sys-monitord.service
│   │   ├── feral-setupd.service
│   │   ├── chromium-kiosk.service
│   │   └── ...
│   ├── controld.json            # Connection daemon config
│   └── watchdog.json            # Watchdog config
└── .bash_profile                # Shell environment
```

#### soaktest User

```
users/soaktest/
├── scripts/                      # Test automation
│   └── soak-test.sh            # Soak testing
├── logs/                        # Test output
├── files/                       # Test assets
└── .bash_profile               # Test environment
```

## Data Flow Architecture

### Component Development Flow

```
Developer → ffos-user/components/ → ffos build process → R2 Storage
```

### User Data Integration Flow

```
ffos-user/users/feralfile/ → ISO /home/feralfile/
ffos-user/users/soaktest/ → ISO /home/soaktest/ (conditional)
```

### Configuration Propagation

```
ffos-user/users/feralfile/.config/ → ISO /home/feralfile/.config/
ffos-user/users/feralfile/scripts/ → ISO /home/feralfile/scripts/
```

## Integration with FFOS

### Version Control

- FFOS references specific ffos-user commits/tags
- Enables reproducible builds
- Supports multiple component versions

### Build Integration

- FFOS checkout ffos-user at specified reference
- Builds components from source
- Merges user data into ISO

### Data Synchronization

```
ffos-user/main → ffos build → R2/{main}/
ffos-user/develop → ffos build → R2/{develop}/
```

## Repository Management

### Commit Guidelines

- Use conventional commit format
- Prefix with component name for clarity
- Example: `feral-controld: add heartbeat functionality`

## Setup Instructions

### No GitHub Actions Required

This repository is purely for source code and data. All build logic is handled by the FFOS repository.
