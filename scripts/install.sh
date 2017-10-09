#!/bin/bash

set -e

(cf uninstall-plugin "logalyzer" || true) && go build -o logalyze-plugin main.go && cf install-plugin -f logalyze-plugin
