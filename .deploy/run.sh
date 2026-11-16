#!/bin/bash
export HOME=/home/hatch
export HERMES_HOME=/home/hatch/.hermes
export PORT=20128
export DATA_DIR=/home/hatch/.9router
export PATH=/opt/hatch/bin:/usr/local/bin:/usr/bin:/bin
cd /home/hatch/workspace/9router-go
exec ./9router-go >> /tmp/9router-go.log 2>&1
