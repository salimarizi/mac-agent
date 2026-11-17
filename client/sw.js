// Minimal service worker. Its job is to make the client installable as a
// standalone app — the only way to get a chrome-less, fullscreen-ish view on iOS,
// which has no Fullscreen API outside <video>.
//
// Deliberately network-first: the client is useless without a live WebSocket to
// the Mac, so a stale cached page would only ever be a worse version of "offline".
// The cache exists purely so a launch from the home screen on a flaky link still
// paints something.

const CACHE = 'mac-remote-v1';
const SHELL = ['./', './index.html', './manifest.webmanifest', './icon-192.png', './icon-512.png'];

self.addEventListener('install', event => {
  event.waitUntil(
    caches.open(CACHE).then(c => c.addAll(SHELL)).then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', event => {
  event.waitUntil(
    caches.keys()
      .then(keys => Promise.all(keys.filter(k => k !== CACHE).map(k => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', event => {
  const req = event.request;
  // Never touch signaling or health; only same-origin GETs are cacheable.
  if (req.method !== 'GET') return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;
  if (url.pathname.endsWith('/ws') || url.pathname.endsWith('/healthz')) return;

  event.respondWith(
    fetch(req)
      .then(res => {
        if (res && res.ok) {
          const copy = res.clone();
          caches.open(CACHE).then(c => c.put(req, copy)).catch(() => {});
        }
        return res;
      })
      .catch(() => caches.match(req).then(hit => hit || caches.match('./index.html')))
  );
});
