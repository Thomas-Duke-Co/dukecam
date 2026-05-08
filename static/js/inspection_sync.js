/**
 * DukeCam Inspection Sync Engine
 *
 * Background sync processor for offline-queued inspection data.
 * Builds on inspection-offline.js (IndexedDB storage layer) and syncs
 * to the server via POST /api/inspections/sync (batch) and individual
 * HTMX endpoints for live inspection status updates.
 *
 * Features:
 * - Online/offline detection with periodic heartbeat
 * - Background sync with exponential backoff retry
 * - Conflict handling via server idempotency keys
 * - Sync panel UI updates
 * - HTMX request interception when offline
 */

// ─── State ───────────────────────────────────────────────────────

let _syncIsOnline = navigator.onLine;
let _syncProcessing = false;
let _syncActiveCount = 0;
const _SYNC_MAX_CONCURRENT = 2;
const _SYNC_RETRY_BASE_MS = 2000;
const _SYNC_MAX_RETRIES = 8;
const _SYNC_HEARTBEAT_INTERVAL = 30000; // 30s

// ─── Connectivity Detection ──────────────────────────────────────

/**
 * Periodic heartbeat to detect real connectivity (navigator.onLine
 * can be stale — it only detects NIC status, not actual server reach).
 */
let _heartbeatTimer = null;

function startHeartbeat() {
    if (_heartbeatTimer) return;
    _heartbeatTimer = setInterval(checkConnectivity, _SYNC_HEARTBEAT_INTERVAL);
}

async function checkConnectivity() {
    if (!navigator.onLine) {
        if (_syncIsOnline) setSyncOnline(false);
        return;
    }
    try {
        const resp = await fetch('/api/health', {
            method: 'HEAD',
            cache: 'no-store',
            signal: AbortSignal.timeout ? AbortSignal.timeout(5000) : undefined,
        });
        if (resp.ok) {
            if (!_syncIsOnline) {
                // Transitioning from offline → online (setSyncOnline triggers sync)
                setSyncOnline(true);
            } else {
                // Already online — periodic retry for any pending work that may have
                // failed due to server errors (not connectivity issues)
                retryPendingIfNeeded();
            }
        }
    } catch {
        if (_syncIsOnline) {
            setSyncOnline(false);
        }
    }
}

/**
 * Periodic retry: if we're online and there's pending work in the queue,
 * trigger a sync cycle. This catches items that failed due to transient
 * server errors (500s, timeouts) rather than connectivity loss.
 */
async function retryPendingIfNeeded() {
    if (_syncProcessing) return; // Already syncing
    if (typeof getSyncQueueStatus !== 'function') return;

    try {
        const status = await getSyncQueueStatus();
        if (status.hasWork) {
            console.log('[InspSync] Periodic retry: found pending work while online');
            triggerSync();
        }
    } catch (err) {
        // Silently ignore — next heartbeat will try again
    }
}

function setSyncOnline(online) {
    const wasOffline = !_syncIsOnline;
    _syncIsOnline = online;

    // Update the inspection conduct page's inline status elements
    updateInspectionSyncUI(online);

    if (online && wasOffline) {
        console.log('[InspSync] Connectivity restored — starting sync');
        triggerSync();
    }
}

window.addEventListener('online', () => {
    console.log('[InspSync] Browser: online');
    setSyncOnline(true);
});

window.addEventListener('offline', () => {
    console.log('[InspSync] Browser: offline');
    setSyncOnline(false);
});

// Also listen for dukecam custom events from base.html
window.addEventListener('dukecam:online', () => setSyncOnline(true));
window.addEventListener('dukecam:offline', () => setSyncOnline(false));

// ─── Sync Panel UI ───────────────────────────────────────────────

function updateInspectionSyncUI(online) {
    const statusBar = document.getElementById('sync-status-bar');
    const statusDot = document.getElementById('sync-status-dot');
    const statusText = document.getElementById('sync-status-text');
    const headerDot = document.getElementById('header-online-dot');
    const headerBadge = document.getElementById('header-sync-badge');
    const syncPanel = document.getElementById('inspection-sync-panel');

    if (online) {
        if (statusBar) {
            statusBar.className = 'flex items-center gap-2 px-3 py-2 rounded-lg text-xs font-medium transition-all duration-300 bg-green-50 text-green-700 border border-green-200';
        }
        if (statusDot) statusDot.className = 'w-2 h-2 rounded-full bg-green-500 flex-shrink-0';
        if (statusText) statusText.textContent = 'Online';
        if (headerDot) {
            headerDot.className = 'inline-block w-1.5 h-1.5 rounded-full bg-green-500 flex-shrink-0';
            headerDot.title = 'Online';
        }
    } else {
        if (statusBar) {
            statusBar.className = 'flex items-center gap-2 px-3 py-2 rounded-lg text-xs font-medium transition-all duration-300 bg-red-50 text-red-700 border border-red-200';
        }
        if (statusDot) statusDot.className = 'w-2 h-2 rounded-full bg-red-500 animate-pulse flex-shrink-0';
        if (statusText) statusText.textContent = 'Offline — changes saved locally';
        if (headerDot) {
            headerDot.className = 'inline-block w-1.5 h-1.5 rounded-full bg-red-500 animate-pulse flex-shrink-0';
            headerDot.title = 'Offline';
        }
        // Show sync panel when offline
        if (syncPanel) syncPanel.classList.remove('hidden');
    }

    // Update pending count badge
    updateSyncBadges();
}

async function updateSyncBadges() {
    if (typeof getSyncQueueStatus !== 'function') return;

    try {
        const status = await getSyncQueueStatus();
        const total = status.pendingDrafts + status.pendingPhotos + status.errorDrafts + status.errorPhotos;

        // Header badge
        const headerBadge = document.getElementById('header-sync-badge');
        if (headerBadge) {
            if (total > 0) {
                headerBadge.textContent = total;
                headerBadge.classList.remove('hidden');
                if (status.errorDrafts + status.errorPhotos > 0) {
                    headerBadge.className = 'hidden text-[10px] font-bold text-red-600 bg-red-50 rounded-full px-1.5 py-0 leading-4';
                    headerBadge.classList.remove('hidden');
                } else {
                    headerBadge.className = 'hidden text-[10px] font-bold text-amber-600 bg-amber-50 rounded-full px-1.5 py-0 leading-4';
                    headerBadge.classList.remove('hidden');
                }
            } else {
                headerBadge.classList.add('hidden');
            }
        }

        // Sync panel pending count
        const pendingCount = document.getElementById('sync-pending-count');
        if (pendingCount) {
            if (total > 0) {
                pendingCount.textContent = total + ' pending';
                pendingCount.classList.remove('hidden');
            } else {
                pendingCount.classList.add('hidden');
            }
        }

        // Show/hide sync panel (show if anything pending or has errors)
        const syncPanel = document.getElementById('inspection-sync-panel');
        if (syncPanel) {
            if (total > 0 || !_syncIsOnline) {
                syncPanel.classList.remove('hidden');
            } else {
                syncPanel.classList.add('hidden');
            }
        }

        // Toggle button and retry all button
        const toggleBtn = document.getElementById('sync-toggle-btn');
        if (toggleBtn) {
            if (total > 0) toggleBtn.classList.remove('hidden');
            else toggleBtn.classList.add('hidden');
        }

        const retryBtn = document.getElementById('sync-retry-all-btn');
        if (retryBtn) {
            if (status.errorDrafts + status.errorPhotos > 0) retryBtn.classList.remove('hidden');
            else retryBtn.classList.add('hidden');
        }

        // Sync items list
        await updateSyncItemsList(status);

        // Update base template's pending count
        if (typeof window.dukecamUpdatePendingCount === 'function') {
            window.dukecamUpdatePendingCount();
        }
    } catch (err) {
        console.error('[InspSync] updateSyncBadges error:', err);
    }
}

async function updateSyncItemsList(status) {
    const list = document.getElementById('sync-items-list');
    if (!list) return;

    const emptyState = document.getElementById('sync-empty-state');
    const total = status.pendingDrafts + status.pendingPhotos + status.errorDrafts + status.errorPhotos;

    if (total === 0) {
        list.innerHTML = '<div id="sync-empty-state" class="px-3 py-4 text-center text-xs text-gray-400">All synced — nothing pending</div>';
        return;
    }

    let html = '';
    if (status.pendingDrafts > 0) {
        html += syncItemHTML('inspection', status.pendingDrafts + ' inspection draft' + (status.pendingDrafts !== 1 ? 's' : ''), 'pending');
    }
    if (status.pendingPhotos > 0) {
        html += syncItemHTML('photo', status.pendingPhotos + ' photo' + (status.pendingPhotos !== 1 ? 's' : ''), 'pending');
    }
    if (status.uploadingPhotos > 0) {
        html += syncItemHTML('upload', status.uploadingPhotos + ' uploading...', 'syncing');
    }
    if (status.errorDrafts > 0) {
        html += syncItemHTML('error', status.errorDrafts + ' failed draft' + (status.errorDrafts !== 1 ? 's' : ''), 'error');
    }
    if (status.errorPhotos > 0) {
        html += syncItemHTML('error', status.errorPhotos + ' failed photo' + (status.errorPhotos !== 1 ? 's' : ''), 'error');
    }

    list.innerHTML = html;
}

function syncItemHTML(icon, text, status) {
    const icons = {
        inspection: '<svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 5H7a2 2 0 00-2 2v12a2 2 0 002 2h10a2 2 0 002-2V7a2 2 0 00-2-2h-2M9 5a2 2 0 002 2h2a2 2 0 002-2M9 5a2 2 0 012-2h2a2 2 0 012 2"/></svg>',
        photo: '<svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M4 16l4.586-4.586a2 2 0 012.828 0L16 16m-2-2l1.586-1.586a2 2 0 012.828 0L20 14m-6-6h.01M6 20h12a2 2 0 002-2V6a2 2 0 00-2-2H6a2 2 0 00-2 2v12a2 2 0 002 2z"/></svg>',
        upload: '<svg class="w-3.5 h-3.5 animate-spin" fill="none" viewBox="0 0 24 24"><circle class="opacity-25" cx="12" cy="12" r="10" stroke="currentColor" stroke-width="4"></circle><path class="opacity-75" fill="currentColor" d="M4 12a8 8 0 018-8v4a4 4 0 00-4 4H4z"></path></svg>',
        error: '<svg class="w-3.5 h-3.5" fill="none" stroke="currentColor" viewBox="0 0 24 24"><path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M12 9v2m0 4h.01m-6.938 4h13.856c1.54 0 2.502-1.667 1.732-2.5L13.732 4c-.77-.833-1.964-.833-2.732 0L4.082 16.5c-.77.833.192 2.5 1.732 2.5z"/></svg>',
    };
    const colors = {
        pending: 'text-amber-600',
        syncing: 'text-duke-teal',
        error: 'text-red-600',
    };
    return `<div class="px-3 py-2 flex items-center gap-2 text-xs ${colors[status] || 'text-gray-500'}">
        ${icons[icon] || icons.inspection}
        <span>${text}</span>
    </div>`;
}

// Toggle sync details panel
function toggleSyncDetails() {
    const details = document.getElementById('sync-details');
    const chevron = document.getElementById('sync-chevron');
    if (details) {
        details.classList.toggle('hidden');
        if (chevron) {
            chevron.style.transform = details.classList.contains('hidden') ? '' : 'rotate(180deg)';
        }
    }
}

// Retry all failed sync items
async function retrySyncAll() {
    if (typeof idbGetAll !== 'function') return;

    try {
        // Reset failed drafts
        const drafts = await idbGetAll(DRAFT_STORE);
        for (const draft of drafts) {
            if (draft.syncStatus === 'error') {
                draft.syncStatus = 'pending';
                await idbPut(DRAFT_STORE, draft);
            }
        }

        // Reset failed photos
        const photos = await idbGetAll(PHOTO_QUEUE_STORE);
        for (const photo of photos) {
            if (photo.syncStatus === 'error') {
                photo.syncStatus = 'pending';
                photo.retries = 0;
                photo.errorMessage = null;
                await idbPut(PHOTO_QUEUE_STORE, photo);
            }
        }

        showSyncNotification('Retrying failed items...', 'success');
        await updateSyncBadges();
        triggerSync();
    } catch (err) {
        console.error('[InspSync] retrySyncAll error:', err);
    }
}

// ─── Background Sync Processor ───────────────────────────────────

/**
 * Main entry point — triggers a sync cycle.
 * Safe to call multiple times (guards against concurrent runs).
 */
function triggerSync() {
    if (!_syncIsOnline) {
        console.log('[InspSync] Offline — skipping sync');
        return;
    }
    if (_syncProcessing) {
        console.log('[InspSync] Sync already in progress');
        return;
    }
    _syncProcessing = true;
    processSyncQueue().finally(() => {
        _syncProcessing = false;
    });
}

/**
 * Process the sync queue: drafts first, then photos.
 * Uses concurrent workers up to _SYNC_MAX_CONCURRENT.
 */
async function processSyncQueue() {
    if (typeof getNextSyncBatch !== 'function') {
        console.warn('[InspSync] Storage layer not loaded (inspection-offline.js missing?)');
        return;
    }

    console.log('[InspSync] Processing sync queue...');
    const batch = await getNextSyncBatch(10);

    // ── Phase 1: Sync draft inspections via batch endpoint ──
    if (batch.drafts.length > 0) {
        await syncDraftBatch(batch.drafts);
    }

    // ── Phase 2: Upload queued photos one at a time ──
    if (batch.photos.length > 0) {
        await syncPhotoBatch(batch.photos);
    }

    await updateSyncBadges();

    // Check if more work remains
    const remaining = await getNextSyncBatch(1);
    if (remaining.drafts.length > 0 || remaining.photos.length > 0) {
        console.log('[InspSync] More items in queue — scheduling next cycle');
        setTimeout(triggerSync, 1000);
    } else {
        console.log('[InspSync] Sync queue empty');
        // Show brief "all synced" notification
        const status = await getSyncQueueStatus();
        if (!status.hasErrors) {
            showSyncNotification('All changes synced ✓', 'success');
        }
    }
}

/**
 * Sync a batch of draft inspections via POST /api/inspections/sync.
 * The server handles idempotency via client_id.
 */
async function syncDraftBatch(drafts) {
    console.log(`[InspSync] Syncing ${drafts.length} draft(s)...`);

    // Build the batch payload
    const inspections = [];
    for (const draft of drafts) {
        try {
            // Convert draft to SyncInspection format
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

            // Convert responses map to array
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
            if (draft.adhocItems && draft.adhocItems.length > 0) {
                for (const adhoc of draft.adhocItems) {
                    syncInsp.adhoc_items.push({
                        label: adhoc.label,
                        category_name: adhoc.categoryName || 'Ad-hoc Items',
                        status: adhoc.status || undefined,
                    });
                }
            }

            // Include any queued photos as base64
            const draftPhotos = await getPhotosForInspection(draft.localId);
            for (const photo of draftPhotos) {
                if (photo.syncStatus !== 'synced' && photo.blob) {
                    const base64 = arrayBufferToBase64(photo.blob);
                    const syncPhoto = {
                        client_photo_id: photo.id,
                        filename: photo.fileName,
                        content_type: photo.mimeType || 'image/jpeg',
                        data_base64: base64,
                        caption: photo.caption || undefined,
                        lat: photo.lat || undefined,
                        lng: photo.lng || undefined,
                    };

                    // Link to item or adhoc
                    if (photo.itemId) syncPhoto.item_id = photo.itemId;
                    if (photo.adhocItemId != null) syncPhoto.adhoc_index = photo.adhocItemId;

                    syncInsp.photos.push(syncPhoto);
                }
            }

            inspections.push({ draft, syncInsp, photos: draftPhotos });
        } catch (err) {
            console.error(`[InspSync] Failed to prepare draft ${draft.localId}:`, err);
        }
    }

    if (inspections.length === 0) return;

    try {
        const resp = await fetch('/api/inspections/sync', {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ inspections: inspections.map(i => i.syncInsp) }),
        });

        if (!resp.ok) {
            throw new Error(`HTTP ${resp.status}: ${await resp.text()}`);
        }

        const result = await resp.json();
        console.log('[InspSync] Batch sync result:', result.summary);

        // Process results
        for (let i = 0; i < result.results.length; i++) {
            const r = result.results[i];
            const inspData = inspections[i];

            if (r.status === 'created' || r.status === 'duplicate') {
                // Mark draft as synced
                await markDraftSynced(inspData.draft.localId, r.inspection_id);

                // Mark associated photos as synced
                for (const photo of inspData.photos) {
                    await markPhotoSynced(photo.id, null);
                }

                console.log(`[InspSync] Draft ${r.client_id} → ${r.status} (inspection_id=${r.inspection_id})`);
            } else if (r.status === 'error') {
                console.error(`[InspSync] Draft ${r.client_id} sync error: ${r.error}`);
                // Mark draft as error
                inspData.draft.syncStatus = 'error';
                await idbPut(DRAFT_STORE, inspData.draft);
            }
        }
    } catch (err) {
        console.error('[InspSync] Batch sync request failed:', err);

        // Mark all drafts back to pending with retry delay
        for (const inspData of inspections) {
            inspData.draft.syncStatus = 'pending';
            await idbPut(DRAFT_STORE, inspData.draft);
        }

        // If network error, mark offline
        if (err.message && (err.message.includes('Network') || err.message.includes('Failed to fetch'))) {
            setSyncOnline(false);
        }
    }
}

/**
 * Upload queued photos one at a time with retry logic.
 * For photos attached to live (already server-synced) inspections.
 */
async function syncPhotoBatch(photos) {
    console.log(`[InspSync] Uploading ${photos.length} queued photo(s)...`);

    for (const photo of photos) {
        if (!_syncIsOnline) {
            console.log('[InspSync] Lost connectivity — stopping photo sync');
            break;
        }

        // Skip photos for drafts that aren't synced yet (they'll go with the batch)
        if (photo.inspectionLocalId && !photo.inspectionServerId) {
            const draft = await getInspectionDraft(photo.inspectionLocalId);
            if (draft && draft.syncStatus !== 'synced') {
                continue; // Will be included in draft batch
            }
        }

        try {
            await markPhotoUploading(photo.id);
            await updateSyncBadges();

            const serverId = photo.inspectionServerId;
            if (!serverId) {
                console.warn(`[InspSync] Photo ${photo.id} has no server inspection ID — skipping`);
                await markPhotoError(photo.id, 'No server inspection ID');
                continue;
            }

            // Build form data for upload
            const blob = new Blob([photo.blob], { type: photo.mimeType || 'image/jpeg' });
            const formData = new FormData();
            formData.append('file', blob, photo.fileName);

            if (photo.itemId) {
                formData.append('item_id', photo.itemId);
            } else if (photo.adhocItemId) {
                formData.append('adhoc_item_id', photo.adhocItemId);
            }
            if (photo.caption) {
                formData.append('caption', photo.caption);
            }

            const resp = await fetch(`/api/inspections/${serverId}/photos`, {
                method: 'POST',
                body: formData,
            });

            if (resp.ok) {
                const data = await resp.json();
                await markPhotoSynced(photo.id, data.id);
                console.log(`[InspSync] Photo ${photo.id} uploaded → server ID ${data.id}`);

                // Update UI: replace queued thumbnail with real one
                const queuedEl = document.getElementById(`queued-photo-${photo.id}`);
                if (queuedEl && data.thumb_url) {
                    queuedEl.className = 'flex-shrink-0 rounded-lg overflow-hidden border border-gray-200';
                    queuedEl.innerHTML = `<img src="${data.thumb_url}" alt="Photo" loading="lazy" class="w-16 h-16 object-cover" />`;
                    if (data.id) {
                        queuedEl.onclick = function() { openLightbox(data.id); };
                        queuedEl.style.cursor = 'pointer';
                    }
                }
            } else if (resp.status === 409) {
                // Duplicate — already uploaded
                console.log(`[InspSync] Photo ${photo.id} already exists on server`);
                await markPhotoSynced(photo.id, null);
            } else {
                throw new Error(`HTTP ${resp.status}`);
            }
        } catch (err) {
            console.error(`[InspSync] Photo upload failed for ${photo.id}:`, err);
            await markPhotoError(photo.id, err.message);

            // Exponential backoff before next photo
            const retryCount = (photo.retries || 0) + 1;
            const delay = _SYNC_RETRY_BASE_MS * Math.pow(2, Math.min(retryCount - 1, 5));
            console.log(`[InspSync] Photo retry ${retryCount} — waiting ${Math.round(delay / 1000)}s`);
            await new Promise(r => setTimeout(r, delay));
        }
    }
}

// ─── HTMX Offline Interceptor ────────────────────────────────────
// When offline, intercept HTMX POST requests for inspection mutations
// and queue them in IndexedDB for later sync.

document.addEventListener('htmx:beforeRequest', function(evt) {
    if (_syncIsOnline) return; // Let HTMX proceed normally when online

    const el = evt.detail.elt;
    const path = evt.detail.pathInfo?.requestPath || evt.detail.requestConfig?.path || '';
    const method = (evt.detail.requestConfig?.verb || 'GET').toUpperCase();

    if (method !== 'POST') return;

    // Match item status updates: /api/inspections/{id}/item/{itemId}/status
    const statusMatch = path.match(/\/api\/inspections\/(\d+)\/item\/(\d+)\/status/);
    if (statusMatch) {
        evt.preventDefault();
        handleOfflineStatusUpdate(parseInt(statusMatch[1]), parseInt(statusMatch[2]), false, el);
        return;
    }

    // Match adhoc item status: /api/inspections/{id}/adhoc/{adhocId}/status
    const adhocStatusMatch = path.match(/\/api\/inspections\/(\d+)\/adhoc\/(\d+)\/status/);
    if (adhocStatusMatch) {
        evt.preventDefault();
        handleOfflineStatusUpdate(parseInt(adhocStatusMatch[1]), parseInt(adhocStatusMatch[2]), true, el);
        return;
    }

    // Match adhoc item creation: /api/inspections/{id}/adhoc
    const adhocMatch = path.match(/\/api\/inspections\/(\d+)\/adhoc$/);
    if (adhocMatch) {
        evt.preventDefault();
        handleOfflineAdhocCreate(parseInt(adhocMatch[1]), el);
        return;
    }

    // Match inspection complete: /api/inspections/{id}/complete
    const completeMatch = path.match(/\/api\/inspections\/(\d+)\/complete/);
    if (completeMatch) {
        evt.preventDefault();
        showSyncNotification('Cannot complete inspection while offline', 'error');
        return;
    }
});

// ─── Fetch-Failure Fallback ─────────────────────────────────────
// When navigator.onLine is true but the fetch actually fails (flaky
// mobile networks, server unreachable), catch the error and queue
// the submission to IndexedDB so no data is lost.

document.addEventListener('htmx:sendError', function(evt) {
    console.warn('[InspSync] HTMX sendError — network failure, routing to offline queue');
    handleFetchFailureFallback(evt);
});

document.addEventListener('htmx:responseError', function(evt) {
    const status = evt.detail.xhr?.status || 0;
    // Only intercept network-level failures (0) and server errors (5xx)
    // Don't intercept 4xx — those are client errors that should surface normally
    if (status === 0 || status >= 500) {
        console.warn('[InspSync] HTMX responseError (status=' + status + ') — routing to offline queue');
        handleFetchFailureFallback(evt);
    }
});

function handleFetchFailureFallback(evt) {
    const el = evt.detail.elt;
    const path = evt.detail.pathInfo?.requestPath || evt.detail.requestConfig?.path || '';
    const method = (evt.detail.requestConfig?.verb || 'GET').toUpperCase();

    if (method !== 'POST') return;

    // Mark connectivity as degraded so subsequent requests go offline immediately
    if (_syncIsOnline) {
        setSyncOnline(false);
        // Schedule a connectivity check to recover once server is reachable
        setTimeout(checkConnectivity, 5000);
    }

    // Route to the same offline handlers used by the beforeRequest interceptor
    const statusMatch = path.match(/\/api\/inspections\/(\d+)\/item\/(\d+)\/status/);
    if (statusMatch) {
        handleOfflineStatusUpdate(parseInt(statusMatch[1]), parseInt(statusMatch[2]), false, el);
        return;
    }

    const adhocStatusMatch = path.match(/\/api\/inspections\/(\d+)\/adhoc\/(\d+)\/status/);
    if (adhocStatusMatch) {
        handleOfflineStatusUpdate(parseInt(adhocStatusMatch[1]), parseInt(adhocStatusMatch[2]), true, el);
        return;
    }

    const adhocMatch = path.match(/\/api\/inspections\/(\d+)\/adhoc$/);
    if (adhocMatch) {
        handleOfflineAdhocCreate(parseInt(adhocMatch[1]), el);
        return;
    }

    const completeMatch = path.match(/\/api\/inspections\/(\d+)\/complete/);
    if (completeMatch) {
        showSyncNotification('Server unreachable — cannot complete inspection. Will retry when connected.', 'error');
        return;
    }
}

async function handleOfflineStatusUpdate(inspectionId, itemId, isAdhoc, el) {
    // Extract status from the element
    let statusValue = '';
    if (el.value) statusValue = el.value;
    else if (el.dataset && el.dataset.status) statusValue = el.dataset.status;
    else if (el.name === 'status') statusValue = el.value;

    // Try to find status from form data
    if (!statusValue) {
        const form = el.closest('form');
        if (form) {
            const fd = new FormData(form);
            statusValue = fd.get('status') || '';
        }
    }

    if (!statusValue) {
        console.warn('[InspSync] Could not determine status value for offline update');
        return;
    }

    // Find or create a draft for this inspection
    if (typeof getInspectionDraftByServerId === 'function') {
        let draft = await getInspectionDraftByServerId(inspectionId);
        if (!draft) {
            // Create a minimal draft to track this response
            draft = await createInspectionDraft({
                serverId: inspectionId,
                propertyId: 0,
                propertyName: 'Unknown (offline)',
                inspectorName: 'Unknown (offline)',
            });
        }

        const itemKey = (isAdhoc ? 'adhoc_' : 'item_') + itemId;
        await updateDraftResponse(draft.localId, itemKey, { status: statusValue });
    }

    // Optimistic UI update
    applyOptimisticStatusUI(itemId, isAdhoc, statusValue);
    showSyncNotification('Saved offline — will sync when connected', 'warning');
    await updateSyncBadges();

    // Request background sync so the browser can retry even if tab is closed
    requestBackgroundSync();
}

async function handleOfflineAdhocCreate(inspectionId, el) {
    const form = el.closest('form') || el;
    const fd = new FormData(form);
    const label = fd.get('label') || '';
    const description = fd.get('description') || '';
    const categoryName = fd.get('category_name') || 'Ad-hoc Items';

    if (!label) return;

    // Find or create a draft
    if (typeof getInspectionDraftByServerId === 'function') {
        let draft = await getInspectionDraftByServerId(inspectionId);
        if (!draft) {
            draft = await createInspectionDraft({
                serverId: inspectionId,
                propertyId: 0,
                propertyName: 'Unknown (offline)',
                inspectorName: 'Unknown (offline)',
            });
        }
        await addDraftAdhocItem(draft.localId, { label, categoryName, notes: description });
    }

    // Optimistic UI: show the item as queued
    const list = document.getElementById('adhoc-items-list');
    if (list) {
        const tempId = 'pending-' + Date.now();
        const escapedLabel = escapeHtmlForSync(label);
        const escapedDesc = description ? escapeHtmlForSync(description) : '';
        list.insertAdjacentHTML('beforeend', `
            <div id="${tempId}" class="bg-white rounded-xl border border-amber-200 p-4 opacity-80">
                <div class="flex items-start gap-3">
                    <div class="flex-1 min-w-0">
                        <p class="text-sm font-medium text-duke-dark">${escapedLabel}</p>
                        ${escapedDesc ? `<p class="text-xs text-gray-400 mt-0.5">${escapedDesc}</p>` : ''}
                        <span class="inline-flex items-center gap-1 mt-1 text-[10px] text-amber-600 font-medium">
                            <svg class="w-3 h-3 animate-pulse" fill="currentColor" viewBox="0 0 20 20"><circle cx="10" cy="10" r="5"/></svg>
                            Queued — will sync when online
                        </span>
                    </div>
                </div>
            </div>
        `);
    }

    // Reset form
    if (form.reset) form.reset();
    const toggleBtn = document.getElementById('adhoc-toggle-btn');
    if (toggleBtn) toggleBtn.classList.remove('hidden');
    if (form.classList) form.classList.add('hidden');

    showSyncNotification('Item saved offline', 'warning');
    await updateSyncBadges();

    // Request background sync
    requestBackgroundSync();
}

// ─── Optimistic UI ───────────────────────────────────────────────

function applyOptimisticStatusUI(itemId, isAdhoc, status) {
    const prefix = isAdhoc ? 'adhoc' : 'item';
    const el = document.getElementById(`${prefix}-${itemId}`);
    if (!el) return;

    // Update status button visual states
    const buttons = el.querySelectorAll('button[data-status]');
    buttons.forEach(btn => {
        const s = btn.dataset.status;
        // Reset all buttons to default
        btn.classList.remove('bg-green-100', 'text-green-700', 'border-green-300',
                            'bg-red-100', 'text-red-700', 'border-red-300',
                            'bg-amber-100', 'text-amber-700', 'border-amber-300',
                            'ring-2', 'ring-offset-1');
        // Highlight the selected one
        if (s === status) {
            if (s === 'pass') btn.classList.add('bg-green-100', 'text-green-700', 'border-green-300', 'ring-2', 'ring-offset-1');
            else if (s === 'fail') btn.classList.add('bg-red-100', 'text-red-700', 'border-red-300', 'ring-2', 'ring-offset-1');
            else if (s === 'needs_attention') btn.classList.add('bg-amber-100', 'text-amber-700', 'border-amber-300', 'ring-2', 'ring-offset-1');
        }
    });

    // Add queued indicator
    let indicator = el.querySelector('.offline-sync-indicator');
    if (!indicator) {
        indicator = document.createElement('span');
        indicator.className = 'offline-sync-indicator inline-flex items-center gap-1 text-[10px] text-amber-600 ml-2';
        indicator.innerHTML = '<svg class="w-3 h-3 animate-pulse" fill="currentColor" viewBox="0 0 20 20"><circle cx="10" cy="10" r="5"/></svg> queued';
        const nameEl = el.querySelector('.text-sm.font-medium, p.text-sm');
        if (nameEl) nameEl.appendChild(indicator);
    }
}

// ─── Offline Photo Upload ────────────────────────────────────────
// Wraps the existing photo capture flow with offline queuing.

/**
 * Upload an inspection photo with offline fallback.
 * If online, attempts direct upload. If offline (or direct fails),
 * queues the photo in IndexedDB for background sync.
 *
 * @param {number} inspectionId - Server inspection ID
 * @param {number} itemId - Item/adhoc item ID
 * @param {boolean} isAdhoc - Whether this is an adhoc item
 * @param {File} file - The photo file
 * @returns {Promise<Object|null>} Server response or null if queued
 */
async function uploadInspectionPhotoOffline(inspectionId, itemId, isAdhoc, file) {
    const itemPrefix = isAdhoc ? 'adhoc' : 'item';
    const itemEl = document.getElementById(`${itemPrefix}-${itemId}`);

    if (_syncIsOnline) {
        // Try direct upload first
        try {
            const formData = new FormData();
            formData.append('file', file);
            if (isAdhoc) {
                formData.append('adhoc_item_id', itemId);
            } else {
                formData.append('item_id', itemId);
            }

            const resp = await fetch(`/api/inspections/${inspectionId}/photos`, {
                method: 'POST',
                body: formData,
            });

            if (resp.ok) {
                return await resp.json();
            }
            throw new Error(`HTTP ${resp.status}`);
        } catch (err) {
            console.warn('[InspSync] Direct photo upload failed, queuing:', err.message);
        }
    }

    // Queue for offline sync
    if (typeof queueInspectionPhoto !== 'function') {
        showSyncNotification('Photo upload failed — offline storage unavailable', 'error');
        return null;
    }

    const queuedPhoto = await queueInspectionPhoto({
        inspectionLocalId: null,
        inspectionServerId: inspectionId,
        itemKey: `${itemPrefix}_${itemId}`,
        itemId: isAdhoc ? null : itemId,
        adhocItemId: isAdhoc ? itemId : null,
        file: file,
    });

    if (queuedPhoto) {
        // Show offline thumbnail preview
        if (itemEl) {
            let strip = itemEl.querySelector('.photo-strip');
            if (!strip) {
                strip = document.createElement('div');
                strip.className = 'photo-strip flex gap-2 mt-2 overflow-x-auto pb-1 -mx-1 px-1';
                const btnRow = itemEl.querySelector('.flex.items-center.gap-2');
                if (btnRow) {
                    itemEl.insertBefore(strip, btnRow);
                } else {
                    itemEl.appendChild(strip);
                }
            }
            const thumbUrl = createPhotoThumbnail(queuedPhoto);
            if (thumbUrl) {
                const thumb = document.createElement('div');
                thumb.id = `queued-photo-${queuedPhoto.id}`;
                thumb.className = 'flex-shrink-0 rounded-lg overflow-hidden border-2 border-dashed border-amber-300 relative';
                thumb.innerHTML = `
                    <img src="${thumbUrl}" alt="Queued" class="w-16 h-16 object-cover opacity-75" />
                    <div class="absolute inset-0 flex items-center justify-center bg-black/20">
                        <svg class="w-4 h-4 text-white animate-pulse" fill="currentColor" viewBox="0 0 20 20"><circle cx="10" cy="10" r="5"/></svg>
                    </div>
                `;
                strip.appendChild(thumb);
            }
        }

        showSyncNotification('Photo saved offline — will upload when connected', 'warning');
        await updateSyncBadges();

        // Request background sync
        requestBackgroundSync();
    }

    return null;
}

// ─── Helpers ─────────────────────────────────────────────────────

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

function escapeHtmlForSync(str) {
    const div = document.createElement('div');
    div.textContent = str || '';
    return div.innerHTML;
}

function showSyncNotification(message, type) {
    // Use existing toast if available
    if (typeof showToast === 'function') {
        showToast(message, type);
        return;
    }
    // Fallback
    let container = document.getElementById('toast-container');
    if (!container) return;

    const colors = { success: 'bg-green-600', error: 'bg-red-600', warning: 'bg-amber-600' };
    const toast = document.createElement('div');
    toast.className = `toast px-4 py-2.5 rounded-lg text-white text-sm font-medium shadow-lg pointer-events-auto ${colors[type] || 'bg-gray-800'}`;
    toast.textContent = message;
    container.appendChild(toast);
    requestAnimationFrame(() => toast.classList.add('show'));
    setTimeout(() => {
        toast.classList.remove('show');
        setTimeout(() => toast.remove(), 300);
    }, 3500);
}

// ─── Synced Entry Cleanup ────────────────────────────────────────

/**
 * Remove fully-synced drafts from IndexedDB after a grace period.
 * Keeps synced drafts for 1 hour (in case user navigates back to review),
 * then purges to free storage. Photos are cleaned up separately by
 * cleanupSyncedPhotos() in inspection-offline.js.
 */
async function cleanupSyncedDrafts() {
    if (typeof idbGetAll !== 'function') return 0;

    try {
        const drafts = await idbGetAll(DRAFT_STORE);
        const now = Date.now();
        const GRACE_PERIOD_MS = 60 * 60 * 1000; // 1 hour
        let cleaned = 0;

        for (const draft of drafts) {
            if (draft.syncStatus === 'synced' && draft.updatedAt && (now - draft.updatedAt) > GRACE_PERIOD_MS) {
                // Also clean up any leftover synced photos for this draft
                if (typeof getPhotosForInspection === 'function') {
                    const photos = await getPhotosForInspection(draft.localId);
                    for (const photo of photos) {
                        if (photo.syncStatus === 'synced') {
                            await idbDelete(PHOTO_QUEUE_STORE, photo.id);
                        }
                    }
                }
                await idbDelete(DRAFT_STORE, draft.localId);
                cleaned++;
            }
        }

        if (cleaned > 0) {
            console.log(`[InspSync] Cleaned up ${cleaned} synced draft(s) past grace period`);
        }
        return cleaned;
    } catch (err) {
        console.error('[InspSync] cleanupSyncedDrafts error:', err);
        return 0;
    }
}

// ─── Visibility Change Handler ──────────────────────────────────

/**
 * When the user returns to the tab (e.g., switches back from another app
 * on mobile), re-check connectivity and trigger sync if needed.
 * This is critical for mobile inspectors who frequently switch between
 * the camera app and DukeCam.
 */
document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') {
        console.log('[InspSync] Tab became visible — checking connectivity');
        // Immediate connectivity check (don't wait for next heartbeat)
        checkConnectivity();
    }
});

// ─── Background Sync API Registration ───────────────────────────

/**
 * Register for the Background Sync API when available.
 * This allows the browser to wake up the service worker and sync
 * even when the tab is closed. Falls back gracefully to heartbeat-only
 * when Background Sync is not supported.
 */
async function registerBackgroundSync() {
    if (!('serviceWorker' in navigator) || !('SyncManager' in window)) {
        console.log('[InspSync] Background Sync API not available — using heartbeat only');
        return;
    }

    try {
        const reg = await navigator.serviceWorker.ready;
        await reg.sync.register('inspection-sync');
        console.log('[InspSync] Background Sync registered: inspection-sync');
    } catch (err) {
        console.warn('[InspSync] Background Sync registration failed:', err.message);
    }
}

/**
 * Request a background sync after queuing new offline data.
 * Called internally when items are added to the queue while offline.
 */
async function requestBackgroundSync() {
    if (!('serviceWorker' in navigator) || !('SyncManager' in window)) return;

    try {
        const reg = await navigator.serviceWorker.ready;
        await reg.sync.register('inspection-sync');
    } catch {
        // Silently fail — heartbeat will handle it
    }
}

// ─── Initialize ──────────────────────────────────────────────────

console.log('[InspSync] Sync engine loading');
startHeartbeat();

// Wait for inspection-offline.js to initialize, then check for pending work
window.addEventListener('load', async () => {
    // Give inspection-offline.js time to initialize
    await new Promise(r => setTimeout(r, 500));

    if (typeof getSyncQueueStatus === 'function') {
        const status = await getSyncQueueStatus();
        updateSyncBadges();

        if (status.hasWork && _syncIsOnline) {
            console.log('[InspSync] Found pending sync items — starting sync');
            triggerSync();
        }
        if (status.hasErrors) {
            console.log(`[InspSync] ${status.errorDrafts + status.errorPhotos} failed items in queue`);
        }

        // Clean up old synced entries to free IndexedDB storage
        await cleanupSyncedDrafts();
    }

    // Register for Background Sync API (progressive enhancement)
    registerBackgroundSync();
});
