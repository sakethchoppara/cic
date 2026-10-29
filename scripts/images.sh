#!/bin/sh
# Pull/build every image in config/languages.yaml.
set -e
cd "$(dirname "$0")/.."
for img in python:3.12-slim node:20-slim gcc:13 rust:1.80-slim; do
	docker pull -q "$img"
done
docker build -q -t cic-go:1.22 -f images/go.Dockerfile images
