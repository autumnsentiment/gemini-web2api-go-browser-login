# New API Image Generation Plugin

This directory runs the image generation page independently from the official
New API image. The gateway proxies every non-plugin request to New API on host
port `4000`, serves the plugin at `/image-gen.html`, and injects a launcher into
the proxied console HTML.

Start it with:

```sh
docker compose up -d
```

Open `http://HOST:4000/` for the proxied New API console or
`http://HOST:4000/image-gen.html` for the standalone page. The official New API
container remains available directly on host port `4001`; public reverse proxies
should keep using port `4000` for same-origin `/v1` API behavior.
