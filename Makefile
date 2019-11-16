# Copyright (c) Microsoft. All rights reserved.
# Licensed under the MIT license. See LICENSE file in the project root for details.

GITCOMMIT=`git rev-parse --short HEAD`
BUILDTIME=`date +%FT%T%z`
HOSTNAME=`hostname`

all: hdfs-mount

hdfs-mount:
	go build -ldflags="-w -X main.GITCOMMIT=${GITCOMMIT} -X main.BUILDTIME=${BUILDTIME} -X main.HOSTNAME=${HOSTNAME}" -o hdfs-mount


clean:
	rm -f hdfs-mount _mock_*.go

mock_%_test.go: %.go | $(MOCKGEN_DIR)/mockgen
	$(MOCKGEN_DIR)/mockgen -source $< -package main > $@~
	mv -f $@~ $@

test: hdfs-mount \
	go test -coverprofile coverage.txt -covermode atomic

debug:
	mkdir -p mountp
	go build -gcflags=all="-N -l" -o hdfs-mount
	@echo "Mount Point: $(PWD)/mountP, port: 2345"
	dlv --listen=:2345 --headless=true --api-version=2 --accept-multiclient exec ./hdfs-mount -- -logLevel 2 master001.earth.clouddev.nm.ted:8020,master002.earth.clouddev.nm.ted:8020 ./mountp
