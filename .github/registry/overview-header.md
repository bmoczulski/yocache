# YoCache

**Smart cache sharing for Yocto builds.**

Sharing Yocto sstate-cache and downloads with your development team? Forget
rsync sessions to an HTTP server. YoCache hooks into your bitbake builds,
uploads artifacts automatically, and serves them back to everyone — one line
of config away.

bitbake can read from an sstate mirror, but it has no built-in way to *push*
to one. That gap is what YoCache exists to close.

- **Documentation:** https://yocache.dev
- **Source & issues:** https://github.com/bmoczulski/yocache

This image is the **server** half: a shared, writable sstate + downloads
mirror with hash-equivalence built in, shipped as a single static binary. The
build-side half is the `meta-yocache` bitbake layer — see "Pointing a build at
it" at the bottom of this page.

---
