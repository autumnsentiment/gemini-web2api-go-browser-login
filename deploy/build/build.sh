#!/bin/sh
set -x
cd /vol2/1000/gw2a-build-415
rm -f build.log src/gemini-web2api
docker run --rm \
  -v /vol2/1000/gw2a-build-415/src:/src \
  -v /vol2/1000/go-mod-cache:/go/pkg/mod \
  -v /vol2/1000/go-build-cache:/root/.cache/go-build \
  -w /src \
  -e GOFLAGS=-mod=mod \
  golang:1.26-alpine sh -c 'go build -buildvcs=false -ldflags="-s -w" -trimpath -o /src/gemini-web2api .' \
  > /vol2/1000/gw2a-build-415/build.log 2>&1
echo "RC=$?" >> /vol2/1000/gw2a-build-415/build.log
ls -la /vol2/1000/gw2a-build-415/src/gemini-web2api >> /vol2/1000/gw2a-build-415/build.log 2>&1
md5sum /vol2/1000/gw2a-build-415/src/gemini-web2api >> /vol2/1000/gw2a-build-415/build.log 2>&1
