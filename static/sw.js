/**
 * DukeCam Service Worker v2.0.0
 *
 * Full offline PWA support:
 * - Precaches app shell (JS, CSS, icons, key pages)
 * - Runtime caching with network-first for pages, cache-first for media
 * - Offline fallback page when network unavailable and page not cached
 * - Background Sync API for inspection data + photo uploads
 */

const SW_VERSION = '2.0.0';
const CACHE_NAME = `dukecam-v${SW_VERSION}`;

// App shell — precached on install
const PRECACHE_URLS = [
    '/',
    '/static/js/tailwind.js',
    '/static/js/htmx.min.js',
    '/static/js/upload.js',
    '/static/js/inspection-offline.js',
    '/static/js/inspection_sync.js',
    '/static/img/tduke-logo.png',
    '/static/img/tduke-favicon.jpg',
    '/static/img/icon-512.png',
    '/static/manifest.json',
    '/offline',
];

// ─── Install: Precache App Shell ────────────────────────────────

self.addEventListener('install', (event) => {
    console.log(`[SW] Installing v${SW_VERSION}`);
    event.waitUntil(
        caches.open(CACHE_NAME).then((cache) => {
            return cache.addAll(PRECACHE_URLS);
        }).then(() => {
            console.log(`[SW] Precache complete (${PRECACHE_URLS.length} assets)`);
            return self.skipWaiting();
        })
    );
});

// ─── Activate: Clean Old Caches ─────────────────────────────────

self.addEventListener('activate', (event) => {
    console.log(`[SW] Activated v${SW_VERSION}`);
    event.waitUntil(
        caches.keys().then((cacheNames) => {
            return Promise.all(
                cacheNames
                    .filter((name) => name.startsWith('dukecam-') && name !== CACHE_NAME)
                    .map((name) => {
                        console.log(`[SW] Deleting old cache: ${name}`);
                        return caches.delete(name);
                    })
            );
        }).then(() => self.clients.claim())
    );
});

// ─── Fetch: Cache Strategies ────────────────────────────────────

self.addEventListener('fetch', (event) => {
    const { request } = event;
    const url = new URL(request.url);

    // Skip non-GET requests (POST uploads, HTMX mutations, etc.)
    if (request.method !== 'GET') return;

    // Skip API calls — these should always go to network
    // (inspection sync, photo upload, health check, etc.)
    if (url.pathname.startsWith('/api/')) return;

    // Static assets: Cache-first (JS, images, manifest)
    if (url.pathname.startsWith('/static/')) {
        event.respondWith(cacheFirst(request));
        return;
    }

    // Media (photos/thumbs): Cache-first with network fallback
    if (url.pathname.startsWith('/media/')) {
        event.respondWith(cacheFirst(request));
        return;
    }

    // Inspection photos: Cache-first
    if (url.pathname.match(/^\/api\/inspections\/photos\//)) {
        event.respondWith(cacheFirst(request));
        return;
    }

    // HTML pages: Network-first with cache fallback, then offline page
    if (request.headers.get('Accept')?.includes('text/html') ||
        url.pathname === '/' ||
        url.pathname.startsWith('/p/') ||
        url.pathname.startsWith('/share/') ||
        url.pathname.startsWith('/admin') ||
        url.pathname.startsWith('/inspection') ||
        url.pathname === '/why' ||
        url.pathname === '/offline') {
        event.respondWith(networkFirstPage(request));
        return;
    }

    // Everything else: Network-first
    event.respondWith(networkFirst(request));
});

/**
 * Cache-first: return from cache if available, otherwise fetch and cache.
 */
async function cacheFirst(request) {
    const cached = await caches.match(request);
    if (cached) return cached;

    try {
        const response = await fetch(request);
        if (response.ok) {
            const cache = await caches.open(CACHE_NAME);
            cache.put(request, response.clone());
        }
        return response;
    } catch (err) {
        // For images, return a transparent 1x1 pixel as fallback
        if (request.url.match(/\.(jpg|jpeg|png|gif|webp)$/i) ||
            request.url.includes('/media/') ||
            request.url.includes('/photos/')) {
            return new Response(
                'data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBRAA7',
                { headers: { 'Content-Type': 'image/gif' } }
            );
        }
        throw err;
    }
}

/**
 * Network-first for pages: try network, cache successful responses,
 * fall back to cache, then offline page.
 */
async function networkFirstPage(request) {
    try {
        const response = await fetch(request);
        if (response.ok) {
            const cache = await caches.open(CACHE_NAME);
            cache.put(request, response.clone());
        }
        return response;
    } catch (err) {
        // Network failed — try cache
        const cached = await caches.match(request);
        if (cached) return cached;

        // Nothing cached — serve offline fallback
        const offlinePage = await caches.match('/offline');
        if (offlinePage) return offlinePage;

        // Last resort
        return new Response('Offline — please reconnect to continue.', {
            status: 503,
            headers: { 'Content-Type': 'text/plain' },
        });
    }
}

/**
 * Network-first for other assets: try network, cache result, fall back to cache.
 */
async function networkFirst(request) {
    try {
        const response = await fetch(request);
        if (response.ok) {
            const cache = await caches.open(CACHE_NAME);
            cache.put(request, response.clone());
        }
        return response;
    } catch (err) {
        const cached = await caches.match(request);
        if (cached) return cached;
        throw err;
    }
}

// ─── Background Sync Handler ────────────────────────────────────

self.addEventListener('sync', (event) => {
    if (event.tag === 'inspection-sync') {
        console.log('[SW] Background sync triggered: inspection-sync');
        event.waitUntil(doBackgroundSync());
    }
    if (event.tag === 'photo-upload-sync') {
        console.log('[SW] Background sync triggered: photo-upload-sync');
        event.waitUntil(doPhotoUploadSync());
    }
});

/**
 * Sync pending photo uploads from IndexedDB upload_queue.
 */
async function doPhotoUploadSync() {
    const DB_NAME = 'dukecam';
    const DB_VERSION = 3;
    let db;
    try {
        db = await openDB(DB_NAME, DB_VERSION);
    } catch (err) {
        console.error('[SW] Could not open IndexedDB for photo sync:', err);
        return;
    }

    try {
        const pending = await getByIndex(db, 'upload_queue', 'status', 'pending');
        console.log(`[SW] Photo upload sync: ${pending.length} pending`);

        for (const item of pending) {
            try {
                const formData = new FormData();
                const blob = new Blob([item.data], { type: item.type || 'image/jpeg' });
                formData.append('file', blob, item.filename);
                if (item.project) formData.append('project', item.project);
                if (item.worker) formData.append('worker', item.worker);
                if (item.caption) formData.append('caption', item.caption);
                if (item.tag) formData.append('tag', item.tag);
                if (item.lat) formData.append('lat', item.lat);
                if (item.lng) formData.append('lng', item.lng);

                const resp = await fetch('/api/upload', {
                    method: 'POST',
                    body: formData,
                });

                if (resp.ok) {
                    item.status = 'uploaded';
                    item.data = null; // Free memory
                    await putItem(db, 'upload_queue', item);
                }
            } catch (err) {
                console.error(`[SW] Photo upload failed: ${item.id}`, err);
                item.retries = (item.retries || 0) + 1;
                await putItem(db, 'upload_queue', item);
            }
        }
    } finally {
        db.close();
    }
}

/**
 * Perform background sync for inspection data.
 */
async function doBackgroundSync() {
    const DB_NAME = 'dukecam';
    const DB_VERSION = 3;
    const DRAFT_STORE = 'inspection_drafts';
    const PHOTO_STORE = 'inspection_photos';

    let db;
    try {
        db = await openDB(DB_NAME, DB_VERSION);
    } catch (err) {
        console.error('[SW] Could not open IndexedDB:', err);
        return;
    }

    try {
        // Phase 1: Sync pending drafts
        const pendingDrafts = await getByIndex(db, DRAFT_STORE, 'syncStatus', 'pending');
        if (pendingDrafts.length > 0) {
            console.log(`[SW] Syncing ${pendingDrafts.length} pending draft(s)`);
            await syncDrafts(db, pendingDrafts, DRAFT_STORE, PHOTO_STORE);
        }

        // Phase 2: Sync pending photos (for already-synced inspections)
        const pendingPhotos = await getByIndex(db, PHOTO_STORE, 'syncStatus', 'pending');
        const orphanPhotos = pendingPhotos.filter(p => p.inspectionServerId);
        if (orphanPhotos.length > 0) {
            console.log(`[SW] Uploading ${orphanPhotos.length} pending photo(s)`);
            await syncPhotos(db, orphanPhotos, PHOTO_STORE);
        }

        console.log('[SW] Background sync complete');
    } catch (err) {
        console.error('[SW] Background sync error:', err);
        throw err; // Causes the browser to retry later
    } finally {
        db.close();
    }
}

// ─── Draft Sync ─────────────────────────────────────────────────

async function syncDrafts(db, drafts, draftStore, photoStore) {
    const inspections = [];

    for (const draft of drafts) {
        const syncInsp = {
            client_id: draft.localId,
            template_id: draft.templateId || undefined,
            property_id: draft.propertyId,
            property_name: draft.propertyName,
            building_id: draft.buildingId || undefined,
            unit_id: draft.unitId || undefined,
            inspector_id: draft.inspectorId || undefined,
            inspector_name: draft.inspectorName,
            notes: draft.notes || undefined,
            complete: draft.status === 'completed',
            responses: [],
            adhoc_items: [],
            photos: [],
        };

        // Convert responses
        if (draft.responses) {
            for (const [key, resp] of Object.entries(draft.responses)) {
                if (key.startsWith('item_')) {
                    const itemId = parseInt(key.replace('item_', ''));
                    if (itemId > 0 && resp.status) {
                        syncInsp.responses.push({
                            item_id: itemId,
                            status: resp.status,
                            notes: resp.notes || undefined,
                        });
                    }
                }
            }
        }

        // Convert adhoc items
        if (draft.adhocItems) {
            for (const adhoc of draft.adhocItems) {
                syncInsp.adhoc_items.push({
                    label: adhoc.label,
                    category_name: adhoc.categoryName || 'Ad-hoc Items',
                    status: adhoc.status || undefined,
                });
            }
        }

        // Include queued photos as base64
        const draftPhotos = await getByIndex(db, photoStore, 'inspectionLocalId', draft.localId);
        for (const photo of draftPhotos) {
            if (photo.syncStatus !== 'synced' && photo.blob) {
                syncInsp.photos.push({
                    client_photo_id: photo.id,
                    filename: photo.fileName,
                    content_type: photo.mimeType || 'image/jpeg',
                    data_base64: arrayBufferToBase64(photo.blob),
                    item_id: photo.itemId || undefined,
                    adhoc_index: photo.adhocItemId != null ? photo.adhocItemId : undefined,
                    caption: photo.caption || undefined,
                    lat: photo.lat || undefined,
                    lng: photo.lng || undefined,
                });
            }
        }

        inspections.push({ draft, syncInsp, photos: draftPhotos });
    }

    if (inspections.length === 0) return;

    const resp = await fetch('/api/inspections/sync', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ inspections: inspections.map(i => i.syncInsp) }),
    });

    if (!resp.ok) {
        throw new Error(`Batch sync failed: HTTP ${resp.status}`);
    }

    const result = await resp.json();
    console.log('[SW] Batch sync result:', result.summary);

    // Update IndexedDB with results
    for (let i = 0; i < result.results.length; i++) {
        const r = result.results[i];
        const inspData = inspections[i];

        if (r.status === 'created' || r.status === 'duplicate') {
            // Mark draft synced
            inspData.draft.serverId = r.inspection_id || inspData.draft.serverId;
            inspData.draft.syncStatus = 'synced';
            inspData.draft.updatedAt = Date.now();
            await putItem(db, draftStore, inspData.draft);

            // Mark photos synced
            for (const photo of inspData.photos) {
                photo.syncStatus = 'synced';
                photo.uploadedAt = Date.now();
                photo.blob = null; // Free memory
                await putItem(db, photoStore, photo);
            }
        } else if (r.status === 'error') {
            inspData.draft.syncStatus = 'error';
            await putItem(db, draftStore, inspData.draft);
        }
    }
}

// ─── Photo Sync ─────────────────────────────────────────────────

async function syncPhotos(db, photos, photoStore) {
    for (const photo of photos) {
        if (!photo.inspectionServerId || !photo.blob) continue;

        try {
            const blob = new Blob([photo.blob], { type: photo.mimeType || 'image/jpeg' });
            const formData = new FormData();
            formData.append('file', blob, photo.fileName);

            if (photo.itemId) formData.append('item_id', photo.itemId);
            else if (photo.adhocItemId) formData.append('adhoc_item_id', photo.adhocItemId);
            if (photo.caption) formData.append('caption', photo.caption);

            const resp = await fetch(`/api/inspections/${photo.inspectionServerId}/photos`, {
                method: 'POST',
                body: formData,
            });

            if (resp.ok || resp.status === 409) {
                photo.syncStatus = 'synced';
                photo.uploadedAt = Date.now();
                photo.blob = null;
                await putItem(db, photoStore, photo);
            } else {
                photo.retries = (photo.retries || 0) + 1;
                if (photo.retries >= 10) {
                    photo.syncStatus = 'error';
                    photo.errorMessage = `HTTP ${resp.status}`;
                }
                await putItem(db, photoStore, photo);
            }
        } catch (err) {
            console.error(`[SW] Photo upload failed: ${photo.id}`, err);
            photo.retries = (photo.retries || 0) + 1;
            await putItem(db, photoStore, photo);
        }
    }
}

// ─── IndexedDB Helpers (Service Worker context) ─────────────────

function openDB(name, version) {
    return new Promise((resolve, reject) => {
        const req = indexedDB.open(name, version);
        req.onsuccess = () => resolve(req.result);
        req.onerror = () => reject(req.error);
        // Don't handle upgradeneeded — SW should only open existing DB
        req.onupgradeneeded = (e) => {
            e.target.transaction.abort();
            reject(new Error('DB needs upgrade — deferring to main thread'));
        };
    });
}

function getByIndex(db, storeName, indexName, value) {
    return new Promise((resolve, reject) => {
        const tx = db.transaction(storeName, 'readonly');
        const req = tx.objectStore(storeName).index(indexName).getAll(value);
        req.onsuccess = () => resolve(req.result || []);
        req.onerror = () => reject(req.error);
    });
}

function putItem(db, storeName, item) {
    return new Promise((resolve, reject) => {
        const tx = db.transaction(storeName, 'readwrite');
        tx.objectStore(storeName).put(item);
        tx.oncomplete = () => resolve();
        tx.onerror = () => reject(tx.error);
    });
}

function arrayBufferToBase64(buffer) {
    let binary = '';
    const bytes = new Uint8Array(buffer);
    const chunkSize = 8192;
    for (let i = 0; i < bytes.length; i += chunkSize) {
        const chunk = bytes.subarray(i, Math.min(i + chunkSize, bytes.length));
        binary += String.fromCharCode.apply(null, chunk);
    }
    return btoa(binary);
}
