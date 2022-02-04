#!/bin/sh

if [ -z "$v" ]; then
	echo "Version number cannot be null. Run with v=[version] release.sh"
	exit 1
fi

cd $(dirname $0)
go install github.com/mitchellh/gox
rm -rf release
mkdir release
cd release

OUTPUT="{{.Dir}}-{{.OS}}-{{.Arch}}-$v"
gox -ldflags "-X main.version=${v}" -os="linux" -output="$OUTPUT" ../cmd/*