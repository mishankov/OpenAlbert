#!/bin/bash
set -euo pipefail
exec python3 "$(dirname "$0")/release.py" build "${1:-release}"
