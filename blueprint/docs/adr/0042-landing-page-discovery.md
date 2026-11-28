# ADR 0042 — Use one canonical address for the landing page

**Status:** Accepted

## Context

GitHub Pages serves the static homepage under `/icaro/`. It is also reachable at `/icaro/index.html`, which was the destination of the site's home link. The Pages workflow publishes the `site/` directory under the project path.

## Decision

Use `https://ggrocco.github.io/icaro/` as the homepage's canonical URL. Use it in the sitemap and repository README, and link to the directory from the site's navigation. Serve the landing page's content and metadata in plain HTML so crawlers can read it on the initial request.

## Consequences

The sitemap ships with the site at `https://ggrocco.github.io/icaro/sitemap.xml`. Google reads `robots.txt` at the host root, so the project can submit its sitemap under a verified URL-prefix property for `https://ggrocco.github.io/icaro/` in [Google Search Console](https://search.google.com/search-console/). If the public address moves, update the canonical link, sitemap, README, and site metadata together.
