# Additional HTTPS sites

`docker-compose.ssl.yml` runs a single Caddy instance on the externally named
`claworc` Docker network. It can route to any container attached to that same
network without publishing the application's port publicly.

To add a host, create `caddy/sites/<hostname>.caddy` next to the root
`Caddyfile`:

```caddy
lifequest.example.com {
    reverse_proxy lifequest:8075
}
```

Then validate and reload Caddy:

```bash
docker compose -f docker-compose.ssl.yml exec caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
docker compose -f docker-compose.ssl.yml exec caddy caddy reload --config /etc/caddy/Caddyfile --adapter caddyfile
```

Requirements:

- The hostname's DNS A/AAAA record must resolve to this server.
- Ports 80 and 443 must remain publicly reachable for ACME challenges and HTTPS.
- The upstream container must join the external `claworc` network. Do not add
  a public `ports:` binding merely for Caddy.
