#!/bin/bash

set -e

if [ "$0" != "./build.sh" ]; then
  echo "build.sh should be run from within the tile directory"
  exit 1
fi

echo "running tests"
pushd ..
go test ./...
popd

echo "building go binary"
pushd ..
env GOOS=linux GOARCH=amd64 go build
popd

echo "building tile"
tile build

