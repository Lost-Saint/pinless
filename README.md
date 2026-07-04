# Pinless

![License: AGPL-3.0](https://img.shields.io/badge/license-AGPL--3.0-blue.svg)
![Go 1.26+](https://img.shields.io/badge/go-1.26%2B-00ADD8?logo=go)

Pinless is a privacy-focused frontend for browsing public Pinterest content.
It lets people search pins, open pin details, and load images without making
direct browser requests to Pinterest or `pinimg.com`.

## Overview

Pinless is a small self-hosted Go application built for people who want a
cleaner way to browse public Pinterest content.

It focuses on:

- keeping Pinterest requests on the server side
- proxying pin images through the app
- using server-rendered pages instead of a heavy frontend
- storing pagination bookmarks in cookies instead of exposing them in URLs

## Demo

### Home

![Pinless home page](https://files.bunk.im/raw/KRfSUF.png)

### Search results

![Pinless search results](https://files.bunk.im/raw/MGU7YJ.jpg)

### Pin details

![Pinless pin details](https://files.bunk.im/raw/zAXXLz.png)

## Features

- Search public Pinterest pins
- Open individual pins with image, title, description, and pinner details
- Browse related pins from a pin detail page
- Proxy image requests through the server
- Use session cookies for pagination bookmarks
- Run as a single Go binary or with Docker

## Tech Stack

- Go 1.26
- [Gin](https://github.com/gin-gonic/gin)
- Embedded HTML templates and static assets
- Docker and Docker Compose for container deployment

## Installation

### Run locally with Go

```bash
git clone https://github.com/Lost-Saint/pinless.git
cd pinless
go run .
```

The server listens on `http://127.0.0.1:3000`.

### Run with Docker Compose

The included [`docker-compose.yml`](./docker-compose.yml) pulls a published
container image and exposes port `3000`.

```bash
docker compose up -d
```

Then open `http://127.0.0.1:3000`.

### Build your own image

```bash
docker build -t pinless .
docker run --rm -p 3000:3000 pinless
```

## Usage

1. Open the app in your browser.
2. Search for a public Pinterest topic or keyword.
3. Open a pin to view its details and related pins.
4. Use the `Next page` link to continue browsing results.

## Instances

Public instances are listed in [`INSTANCES.md`](./INSTANCES.md).

- Directory: https://instances.bunk.im

## Privacy Notes

- Pinless only works with public Pinterest content.
- The browser talks to your Pinless server, not directly to Pinterest for
  search and image requests.
- Pagination state is stored in session cookies to avoid leaking bookmark
  tokens through URLs, browser history, or referrers.

## Contributing

Pull requests are welcome. If you plan to make a larger change, open an issue
first so the behavior and scope are clear before implementation.

## License

This project is licensed under the GNU Affero General Public License v3.0.
See [`LICENSE`](./LICENSE) for the full text.
