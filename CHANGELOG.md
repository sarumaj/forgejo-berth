# Changelog

All notable changes to this project are documented in this file. The release bot
writes each version's section from the commits (Conventional Commits) and the hand-written
Unreleased section.

## 0.2.1 - 2026-11-23

### Fixed

- a global runner may take any owner's job, and a new runner reserves for claimed ones

## 0.2.0 - 2026-11-23

### Added

- **incus:** a provider that reaches Incus containers through the Incus API
- **docker:** a provider whose slots are containers on a Docker Engine

### Changed

- **slot:** Incus's container operations behind an Instance
- **slot:** the slot side runs in the broker too, and its cycle resumes

### Fixed

- **docker:** the runner's log at debug level, as the incus journal
- shadow mode no longer reports the live instance's lost runners as its own

## 0.1.0 - 2026-11-23

### Added

- **cycle:** a failed boot says why
- the metrics as a textfile, skipped jobs logged
- the broker, its SSH slot provider, the slot command and the reset cycle
