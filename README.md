# mautrix-syncproxy
A "proxy" microservice that runs Matrix C-S `/sync` on the server and pushes
to-device events and other encryption-related things through the usual
appservice transaction system.

This was designed for use with [mautrix-wsproxy] and the [android-sms] bridge
to significantly reduce battery usage of the persistent foreground process
(an idle websocket doesn't take much battery, but a HTTP request every 30
seconds does).

This partially implements [MSC3202] and the to-device part of [MSC2409].

[MSC2409]: https://github.com/matrix-org/matrix-doc/pull/2409
[MSC3202]: https://github.com/matrix-org/matrix-doc/pull/3202
[android-sms]: https://gitlab.com/beeper/android-sms
[mautrix-wsproxy]: https://github.com/mautrix/wsproxy

## Setup
You can download a prebuilt executable from [the CI] or [GitHub releases]. The
executables are statically compiled and have no dependencies. Alternatively,
you can build from source:

[the CI]: https://mau.dev/mautrix/syncproxy/-/pipelines
[GitHub releases]: https://github.com/mautrix/syncproxy/releases

0. Have [Go](https://golang.org/) 1.16 or higher installed.
1. Clone the repository (`git clone https://github.com/mautrix/syncproxy.git`).
2. Build with `go build -o mautrix-syncproxy`. The resulting executable will be
   in the current directory named `mautrix-syncproxy`.

Configuring is done via environment variables.

* `LISTEN_ADDRESS` - The address where to listen.
* `HOMESERVER_URL` - The address to Synapse. If using workers, it is sufficient
  to have access to the `GET /sync` and `POST /user/{userId}/filter` endpoints.
* `DATABASE_URL` - Database for storing sync tokens. SQLite and Postgres are
  supported: `sqlite:///mautrix-syncproxy.db` or `postgres://user:pass@host/db`
* `SHARED_SECRET` - The shared secret for adding new sync targets.
  You should generate a random string here, e.g. `pwgen -snc 50 1`
* `DEBUG` - If set, debug logs will be enabled.

Since this is most useful with mautrix-wsproxy, the docker-compose instructions
can be found in the [mautrix-wsproxy] readme.
