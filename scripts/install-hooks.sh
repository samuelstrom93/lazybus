#!/usr/bin/env bash
# Point git at the versioned hooks in .githooks/.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
chmod +x .githooks/*
git config core.hooksPath .githooks
echo "hooks installed: core.hooksPath=.githooks"
