# mautrix-syncproxy
A "proxy" microservice that runs Matrix C-S `/sync` on the server and pushes
to-device events and other encryption-related things through the usual
appservice transaction system.

This was designed for use with [mautrix-wsproxy] and the [android-sms] bridge
to significantly reduce battery usage of the persistent foreground process
(an idle websocket doesn't take much battery, but a HTTP request every 30
seconds does).

This implements [MSC3202] and the to-device part of [MSC2409].

[MSC2409]: https://github.com/matrix-org/matrix-doc/pull/2409
[MSC3202]: https://github.com/matrix-org/matrix-doc/pull/3202
[mautrix-wsproxy]: https://github.com/tulir/mautrix-wsproxy
[android-sms]: https://gitlab.com/beeper/android-sms
