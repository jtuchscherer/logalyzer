#!/bin/bash

set -e

function cleanup {
  rm -f main.exe
  rm -f logalyze-plugin
}
trap cleanup EXIT

SCRIPT_DIR=`dirname $0`
cd ${SCRIPT_DIR}/..

echo "Go formatting..."
go fmt ./...

echo "Go vetting..."
go vet ./...
