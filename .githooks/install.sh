#!/bin/sh
# Point git at the tracked .githooks/ dir so the pre-commit hook runs for everyone
# who installs it. Run once after cloning: ./.githooks/install.sh
set -e
cd "$(git rev-parse --show-toplevel)"
chmod +x .githooks/pre-commit
git config core.hooksPath .githooks
echo "installed: core.hooksPath -> .githooks (pre-commit active)"
