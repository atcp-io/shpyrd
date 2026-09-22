#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
kind create cluster --config=kind.yml
