/* MiniCRM Service Worker: static shell precache and offline navigation fallback only.
 * Never caches authenticated HTML, email bodies, or attachments.
 */
var CACHE_NAME = "minicrm-shell-v3";
var SHELL_ASSETS = [
  "/static/app.css",
  "/static/app.js",
  "/static/htmx.min.js",
  "/static/editor.js",
  "/static/favicon.svg",
  "/static/icon-192.png",
  "/static/icon-512.png",
  "/static/icon-maskable-512.png",
  "/static/apple-touch-icon.png",
  "/static/offline.html"
];

self.addEventListener("install", function (event) {
  event.waitUntil(
    caches.open(CACHE_NAME).then(function (cache) {
      return cache.addAll(SHELL_ASSETS);
    }).then(function () {
      return self.skipWaiting();
    })
  );
});

self.addEventListener("activate", function (event) {
  event.waitUntil(
    caches.keys().then(function (keys) {
      return Promise.all(
        keys.map(function (key) {
          if (key !== CACHE_NAME && key.indexOf("minicrm-shell-") === 0) {
            return caches.delete(key);
          }
          return undefined;
        })
      );
    }).then(function () {
      return self.clients.claim();
    })
  );
});

self.addEventListener("fetch", function (event) {
  var request = event.request;
  if (request.method !== "GET") {
    return;
  }

  var url = new URL(request.url);
  if (url.origin !== self.location.origin) {
    return;
  }

  // Never intercept OIDC authentication flows, isolated email frames, or attachments.
  if (
    url.pathname.indexOf("/auth/") === 0 ||
    url.pathname.indexOf("/emails/") === 0 ||
    url.pathname.indexOf("/attachments/") === 0
  ) {
    return;
  }

  // Static shell assets: network-first with cache fallback.
  if (url.pathname.indexOf("/static/") === 0) {
    event.respondWith(
      fetch(request)
        .then(function (response) {
          if (response && response.status === 200) {
            var clone = response.clone();
            caches.open(CACHE_NAME).then(function (cache) {
              cache.put(request, clone);
            });
          }
          return response;
        })
        .catch(function () {
          return caches.match(request);
        })
    );
    return;
  }

  // Top-level page navigations: network-only, falling back to static offline notice on network failure.
  if (request.mode === "navigate") {
    event.respondWith(
      fetch(request).catch(function () {
        return caches.match("/static/offline.html");
      })
    );
  }
});
