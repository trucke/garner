# Security

## Report a vulnerability

Report security problems privately through [GitHub's private vulnerability reporting](https://github.com/trucke/garner/security/advisories/new), not in a public issue. Describe the problem, the version you use and, if you can, the steps to reproduce it.

You get an answer within a week. Garner is maintained in spare time, so a fix can take longer; the advisory keeps you informed.

## Supported versions

Only the newest release gets security fixes.

## What Garner protects

- Clients authenticate with a token, which `garnerd` stores only as a hash.
- Clients that connect to Garner directly beyond loopback get HTTPS only. With its own certificate, the connection code carries the certificate's key pin, and clients refuse a server with another key. Behind a reverse proxy (`proxied = true`), the proxy holds a publicly trusted certificate and Garner serves plain HTTP, which only the proxy may reach; firewall the listener.
- Admin commands such as `garnerd token create` reach the server through a socket in the data folder, not over the network.
- The container runs as UID 1000 on a read-only root filesystem. Its FFprobe reads only the video formats Garner checks, with no network access and no decoders.
