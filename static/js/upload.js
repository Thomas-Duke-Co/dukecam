/**
 * DukeCam Upload Engine v2
 * 
 * Bulletproof upload with:
 * - IndexedDB queue (survives browser close)
 * - Fallback direct upload if IndexedDB unavailable
 * - Automatic retry with exponential backoff
 * - Multi-file concurrent upload
 * - Progress tracking per file
 * - Offline detection & resume
 * - Visibility change detection (camera return fallback)
 * - Never blocks upload — worker optional
 */

const DB_NAME = 'dukecam';
const DB_VERSION = 3;
const STORE_NAME = 'upload_queue';
const MAX_CONCURRENT = 2;
const MAX_RETRIES = 10;
const RETRY_BASE_MS = 2000;

let db = null;
let dbAvailable = false;
let activeUploads = 0;
let isProcessing = false;

// ─── Batch progress tracker ──────────────────────────────────────
// Tracks per-batch counts (selected/queued/uploaded/failed) and renders
// a sticky banner so users see live progress on 100+ photo batches.

const batchTracker = (function() {
    const batches = new Map();

    function get(id) {
        let b = batches.get(id);
        if (!b) {
            b = { id, total: 0, queued: 0, uploaded: 0, failed: 0 };
            batches.set(id, b);
        }
        return b;
    }

    function render() {
        const container = document.getElementById('upload-queue');
        if (!container) return;
        let banner = document.getElementById('batch-progress-banner');

        // Aggregate across all active batches.
        let total = 0, queued = 0, uploaded = 0, failed = 0;
        for (const b of batches.values()) {
            total += b.total;
            queued += b.queued;
            uploaded += b.uploaded;
            failed += b.failed;
        }

        const pending = queued - uploaded - failed;
        const done = total > 0 && (uploaded + failed >= total) && pending <= 0;

        if (total === 0) {
            if (banner) banner.remove();
            return;
        }

        if (!banner) {
            banner = document.createElement('div');
            banner.id = 'batch-progress-banner';
            banner.style.cssText = 'position:sticky;top:0;z-index:50;background:#0f766e;color:#fff;padding:10px 14px;border-radius:10px;font-weight:600;font-size:14px;margin-bottom:8px;box-shadow:0 2px 8px rgba(0,0,0,.15);display:flex;align-items:center;gap:10px;';
            container.style.display = 'block';
            container.prepend(banner);
        }

        const processed = uploaded + failed;
        const pctNum = total > 0 ? Math.round((processed / total) * 100) : 0;
        const failedTxt = failed > 0 ? ` · <span style="color:#fecaca">${failed} failed</span>` : '';
        const icon = done ? (failed > 0 ? '⚠️' : '✅') : '📤';
        const label = done
            ? (failed > 0 ? `Done with ${failed} failed of ${total}` : `Uploaded ${uploaded} of ${total}`)
            : `Uploading ${uploaded} of ${total}${failedTxt}`;

        banner.innerHTML = `
            <span style="font-size:18px">${icon}</span>
            <div style="flex:1">
                <div>${label}</div>
                <div style="height:6px;background:rgba(255,255,255,.25);border-radius:3px;margin-top:6px;overflow:hidden">
                    <div style="height:100%;width:${pctNum}%;background:#fff;transition:width .3s"></div>
                </div>
            </div>
            <span style="font-variant-numeric:tabular-nums;font-size:13px;opacity:.9">${pctNum}%</span>
        `;

        if (done) {
            setTimeout(() => {
                // Clear completed batches after a moment.
                for (const [id, b] of batches) {
                    if (b.uploaded + b.failed >= b.total) batches.delete(id);
                }
                render();
            }, 4000);
        }
    }

    return {
        start(id, total) {
            const b = get(id);
            b.total += total;
            render();
        },
        recordQueued(id) { if (id && batches.has(id)) { get(id).queued++; render(); } },
        recordUploaded(id) { if (id && batches.has(id)) { get(id).uploaded++; render(); } },
        recordFailed(id) { if (id && batches.has(id)) { get(id).failed++; render(); } },
    };
})();

// ─── IndexedDB Setup ─────────────────────────────────────────────

async function openDB() {
    try {
        return await new Promise((resolve, reject) => {
            const req = indexedDB.open(DB_NAME, DB_VERSION);
            req.onupgradeneeded = (e) => {
                const database = e.target.result;
                if (!database.objectStoreNames.contains(STORE_NAME)) {
                    const store = database.createObjectStore(STORE_NAME, { keyPath: 'id' });
                    store.createIndex('status', 'status', { unique: false });
                }
                // v3: Inspection offline stores (also created by inspection-offline.js)
                if (!database.objectStoreNames.contains('inspection_drafts')) {
                    const draftStore = database.createObjectStore('inspection_drafts', { keyPath: 'localId' });
                    draftStore.createIndex('serverId', 'serverId', { unique: false });
                    draftStore.createIndex('status', 'status', { unique: false });
                    draftStore.createIndex('syncStatus', 'syncStatus', { unique: false });
                    draftStore.createIndex('updatedAt', 'updatedAt', { unique: false });
                }
                if (!database.objectStoreNames.contains('inspection_photos')) {
                    const photoStore = database.createObjectStore('inspection_photos', { keyPath: 'id' });
                    photoStore.createIndex('inspectionLocalId', 'inspectionLocalId', { unique: false });
                    photoStore.createIndex('syncStatus', 'syncStatus', { unique: false });
                    photoStore.createIndex('itemKey', 'itemKey', { unique: false });
                    photoStore.createIndex('queuedAt', 'queuedAt', { unique: false });
                }
            };
            req.onsuccess = (e) => {
                db = e.target.result;
                dbAvailable = true;
                console.log('[DukeCam] IndexedDB ready');
                resolve(db);
            };
            req.onerror = (e) => {
                console.error('[DukeCam] IndexedDB failed to open:', e.target.error);
                reject(e.target.error);
            };
        });
    } catch (err) {
        console.error('[DukeCam] IndexedDB unavailable, using direct upload fallback');
        dbAvailable = false;
        return null;
    }
}

function dbPut(item) {
    if (!dbAvailable) return Promise.resolve();
    return new Promise((resolve, reject) => {
        try {
            const tx = db.transaction(STORE_NAME, 'readwrite');
            tx.objectStore(STORE_NAME).put(item);
            tx.oncomplete = () => resolve();
            tx.onerror = (e) => { console.error('[DukeCam] dbPut error:', e.target.error); reject(e.target.error); };
        } catch (err) {
            console.error('[DukeCam] dbPut exception:', err);
            reject(err);
        }
    });
}

function dbDelete(id) {
    if (!dbAvailable) return Promise.resolve();
    return new Promise((resolve, reject) => {
        try {
            const tx = db.transaction(STORE_NAME, 'readwrite');
            tx.objectStore(STORE_NAME).delete(id);
            tx.oncomplete = () => resolve();
            tx.onerror = (e) => reject(e.target.error);
        } catch (err) {
            console.error('[DukeCam] dbDelete exception:', err);
            resolve(); // Don't block on cleanup failure
        }
    });
}

function dbGetPending() {
    if (!dbAvailable) return Promise.resolve([]);
    return new Promise((resolve, reject) => {
        try {
            const tx = db.transaction(STORE_NAME, 'readonly');
            const store = tx.objectStore(STORE_NAME);
            const idx = store.index('status');
            const req = idx.getAll('pending');
            req.onsuccess = () => resolve(req.result);
            req.onerror = (e) => { console.error('[DukeCam] dbGetPending error:', e.target.error); resolve([]); };
        } catch (err) {
            console.error('[DukeCam] dbGetPending exception:', err);
            resolve([]);
        }
    });
}

// ─── Queue Management ────────────────────────────────────────────

async function enqueueFiles(files, projectId, workerId, workerName, caption, tag) {
    const total = files.length;
    console.log(`[DukeCam] enqueueFiles: ${total} files, project=${projectId}, worker=${workerId || workerName || 'none'}, tag=${tag}`);
    const batchId = (crypto.randomUUID ? crypto.randomUUID() : Date.now().toString());
    let readFailures = 0;
    let quotaFailures = 0;
    let enqueued = 0;

    batchTracker.start(batchId, total);

    // Process one file at a time, reading its bytes into an ArrayBuffer
    // and immediately persisting to IndexedDB so the queued item is
    // INDEPENDENT of the source <input> element (iOS Safari invalidates
    // File refs the moment input.value is cleared — that's why earlier
    // versions silently lost everything past the first concurrent batch).
    //
    // We yield between files so a 100+ photo batch:
    //   (a) never holds more than ONE photo's bytes in the JS heap at once
    //       (the local `buffer` var is replaced + GC'd each iteration), and
    //   (b) lets iOS repaint and keeps the script-too-long watchdog at bay.
    const fileArr = Array.from(files);
    for (let i = 0; i < fileArr.length; i++) {
        const file = fileArr[i];
        const sizeMB = (file && file.size ? (file.size / 1024 / 1024).toFixed(1) : '?');
        console.log(`[DukeCam] Enqueue ${i + 1}/${total}: ${file && file.name} (${sizeMB}MB, ${file && file.type || 'unknown'})`);

        if (!file || file.size === 0) {
            console.warn(`[DukeCam] Empty/unreadable file ${file && file.name} — skipping`);
            readFailures++;
            batchTracker.recordFailed(batchId);
            continue;
        }

        let buffer;
        try {
            buffer = await file.arrayBuffer();
        } catch (err) {
            console.error(`[DukeCam] Failed to read ${file.name}:`, err);
            readFailures++;
            batchTracker.recordFailed(batchId);
            continue;
        }

        if (!buffer || buffer.byteLength === 0) {
            console.warn(`[DukeCam] Zero-byte read on ${file.name} — iCloud placeholder?`);
            readFailures++;
            batchTracker.recordFailed(batchId);
            buffer = null;
            continue;
        }

        const item = {
            id: `${batchId}-${Date.now()}-${i}-${Math.random().toString(36).slice(2, 8)}`,
            blob: buffer, // ArrayBuffer — independent of the source <input>
            fileName: file.name || `photo-${Date.now()}.jpg`,
            fileSize: buffer.byteLength,
            mimeType: file.type || 'image/jpeg',
            projectId,
            workerId: workerId || null,
            workerName: workerName || null,
            caption: caption || null,
            tag: tag || null,
            batchId,
            status: 'pending',
            retries: 0,
            addedAt: Date.now(),
        };

        try {
            if (dbAvailable) {
                await dbPut(item);
                renderQueueItem(item);
                enqueued++;
                batchTracker.recordQueued(batchId);
            } else {
                renderQueueItem(item);
                enqueued++;
                batchTracker.recordQueued(batchId);
                directUpload(item);
            }
        } catch (err) {
            const name = (err && err.name) || '';
            const msg = (err && err.message) || String(err);
            console.error(`[DukeCam] Enqueue failed for ${file.name}: ${name} ${msg}`);
            if (name === 'QuotaExceededError' || /quota/i.test(msg)) {
                quotaFailures++;
            } else {
                readFailures++;
            }
            batchTracker.recordFailed(batchId);
        }

        // Drop the JS-heap reference so the GC can reclaim this photo's
        // bytes before we read the next one.
        buffer = null;
        item.blob = null;

        // Yield between every file so iOS Safari stays responsive and
        // the GC has a chance to run.
        if ((i & 3) === 3) {
            // Every 4th file, yield via rAF (heavier, lets paint happen).
            await new Promise(r => (window.requestAnimationFrame || setTimeout)(r, 0));
        } else {
            // Otherwise, just a microtask yield.
            await Promise.resolve();
        }

        // Kick the uploader as soon as 2 items are queued so uploads run
        // concurrently with enqueueing (don't wait for all 110 to read).
        if (i === 1 && dbAvailable) processQueue();
    }

    if (quotaFailures > 0) {
        showToast(`Storage full — ${quotaFailures} photo${quotaFailures > 1 ? 's' : ''} dropped. Upload current batch first, then add more.`, 'error');
        console.warn(`[DukeCam] ${quotaFailures} file(s) dropped by IndexedDB quota`);
    }
    if (readFailures > 0) {
        const msg = readFailures === total
            ? `Could not read ${readFailures} photo${readFailures > 1 ? 's' : ''} — photos may not be downloaded from cloud yet`
            : `${readFailures} of ${total} photos could not be read — they may not be downloaded from cloud yet`;
        showToast(msg, 'error');
        console.warn(`[DukeCam] ${readFailures} file(s) failed to read`);
    }

    console.log(`[DukeCam] Enqueue complete: ${enqueued}/${total} queued (${readFailures} unreadable, ${quotaFailures} quota)`);

    if (dbAvailable) {
        registerPhotoSync();
        processQueue();
    }
}

async function registerPhotoSync() {
    if ('serviceWorker' in navigator && 'SyncManager' in window) {
        try {
            const reg = await navigator.serviceWorker.ready;
            await reg.sync.register('photo-upload-sync');
            console.log('[DukeCam] Background Sync registered: photo-upload-sync');
        } catch (err) {
            console.warn('[DukeCam] Background Sync registration failed:', err);
        }
    }
}

async function processQueue() {
    if (!dbAvailable) return;
    if (isProcessing) {
        console.log(`[DukeCam] processQueue: already processing`);
        return;
    }
    isProcessing = true;
    console.log(`[DukeCam] processQueue: starting`);

    try {
        while (true) {
            if (activeUploads >= MAX_CONCURRENT) {
                console.log(`[DukeCam] processQueue: at max concurrent (${activeUploads}/${MAX_CONCURRENT})`);
                break;
            }

            const pending = await dbGetPending();
            console.log(`[DukeCam] processQueue: ${pending.length} pending`);
            if (pending.length === 0) break;

            pending.sort((a, b) => a.addedAt - b.addedAt);
            const item = pending[0];

            item.status = 'uploading';
            await dbPut(item);
            updateQueueItem(item.id, 'uploading', 'Uploading...');

            activeUploads++;
            uploadItem(item).finally(() => {
                activeUploads--;
                processQueue();
            });
        }
    } catch (err) {
        console.error('[DukeCam] processQueue error:', err);
    }

    isProcessing = false;
}

// ─── Upload (queued) ─────────────────────────────────────────────

async function uploadItem(item) {
    console.log(`[DukeCam] uploadItem: ${item.id} (${item.fileName}, ${item.blob.byteLength} bytes)`);
    try {
        const result = await doUpload(item);

        console.log(`[DukeCam] Upload success: ${item.fileName} → id=${result.id}`);
        await dbDelete(item.id);
        updateQueueItem(item.id, 'done', '✓ Uploaded');
        batchTracker.recordUploaded(item.batchId);
        fadeOutQueueItem(item.id);

    } catch (err) {
        console.error(`[DukeCam] Upload failed: ${item.fileName}`, err);
        item.retries++;
        if (item.retries >= MAX_RETRIES) {
            item.status = 'failed';
            await dbPut(item);
            updateQueueItem(item.id, 'error', `Failed after ${MAX_RETRIES} attempts`);
            batchTracker.recordFailed(item.batchId);
            showToast(`Upload failed: ${item.fileName} — tap Retry`, 'error');
        } else {
            item.status = 'pending';
            await dbPut(item);
            const delay = RETRY_BASE_MS * Math.pow(2, Math.min(item.retries - 1, 5));
            updateQueueItem(item.id, 'retrying', `Retry ${item.retries}/${MAX_RETRIES} in ${Math.round(delay / 1000)}s...`);
            await sleep(delay);
        }
    }
}

// ─── Upload (direct fallback, no IndexedDB) ──────────────────────

async function directUpload(item) {
    console.log(`[DukeCam] directUpload: ${item.fileName}`);
    updateQueueItem(item.id, 'uploading', 'Uploading...');

    let retries = 0;
    while (retries < MAX_RETRIES) {
        try {
            const result = await doUpload(item);
            console.log(`[DukeCam] Direct upload success: ${item.fileName} → id=${result.id}`);
            updateQueueItem(item.id, 'done', '✓ Uploaded');
            batchTracker.recordUploaded(item.batchId);
            fadeOutQueueItem(item.id);
            return;
        } catch (err) {
            retries++;
            console.error(`[DukeCam] Direct upload attempt ${retries} failed:`, err);
            if (retries >= MAX_RETRIES) {
                updateQueueItem(item.id, 'error', `Failed after ${MAX_RETRIES} attempts`);
                batchTracker.recordFailed(item.batchId);
                showToast(`Upload failed: ${item.fileName}`, 'error');
            } else {
                const delay = RETRY_BASE_MS * Math.pow(2, Math.min(retries - 1, 5));
                updateQueueItem(item.id, 'retrying', `Retry ${retries}/${MAX_RETRIES}...`);
                await sleep(delay);
            }
        }
    }
}

// ─── Shared upload logic (XHR) ───────────────────────────────────

function doUpload(item) {
    // item.blob may be a Blob/File (new code path) or an ArrayBuffer
    // (legacy queue entries from before the streaming rewrite).
    let blob;
    if (item.blob instanceof Blob) {
        blob = item.blob;
    } else {
        blob = new Blob([item.blob], { type: item.mimeType || 'image/jpeg' });
    }
    const formData = new FormData();
    formData.append('file', blob, item.fileName || 'photo.jpg');
    formData.append('project_id', item.projectId);
    if (item.workerId) formData.append('worker_id', item.workerId);
    if (item.workerName) formData.append('worker_name', item.workerName);
    if (item.caption) formData.append('caption', item.caption);
    if (item.tag) formData.append('tag', item.tag);
    formData.append('batch_id', item.batchId || 'direct');

    return new Promise((resolve, reject) => {
        const xhr = new XMLHttpRequest();
        xhr.open('POST', '/api/upload');
        xhr.timeout = 120000;

        xhr.upload.onprogress = (e) => {
            if (e.lengthComputable) {
                const pct = Math.round((e.loaded / e.total) * 100);
                updateQueueProgress(item.id, pct);
            }
        };

        xhr.onload = () => {
            console.log(`[DukeCam] XHR response: ${xhr.status} ${xhr.responseText.substring(0, 200)}`);
            if (xhr.status >= 200 && xhr.status < 300) {
                try {
                    resolve(JSON.parse(xhr.responseText));
                } catch (e) {
                    reject(new Error(`Bad response: ${xhr.responseText.substring(0, 100)}`));
                }
            } else {
                reject(new Error(`HTTP ${xhr.status}: ${xhr.responseText.substring(0, 200)}`));
            }
        };

        xhr.onerror = () => {
            console.error('[DukeCam] XHR network error');
            reject(new Error('Network error'));
        };
        xhr.ontimeout = () => {
            console.error('[DukeCam] XHR timeout (120s)');
            reject(new Error('Timeout'));
        };

        xhr.send(formData);
    });
}

function sleep(ms) {
    return new Promise(r => setTimeout(r, ms));
}

// ─── Resume on page load ─────────────────────────────────────────

async function resumePendingUploads() {
    if (!dbAvailable) return;

    try {
        const tx = db.transaction(STORE_NAME, 'readwrite');
        const store = tx.objectStore(STORE_NAME);
        const req = store.getAll();

        req.onsuccess = async () => {
            const items = req.result;
            let hasPending = false;
            for (const item of items) {
                if (item.status === 'uploading') {
                    item.status = 'pending';
                    await dbPut(item);
                }
                if (item.status === 'pending' || item.status === 'uploading') {
                    renderQueueItem(item);
                    hasPending = true;
                }
                if (item.status === 'failed') {
                    renderQueueItem(item);
                    updateQueueItem(item.id, 'error', 'Failed — tap Retry');
                }
            }
            if (hasPending) {
                showToast('Resuming uploads...', '');
                processQueue();
            }
        };
    } catch (err) {
        console.error('[DukeCam] resumePendingUploads error:', err);
    }
}

// ─── Online/Offline detection ────────────────────────────────────

window.addEventListener('online', () => {
    console.log('[DukeCam] Back online');
    showToast('Back online — resuming uploads', 'success');
    processQueue();
});

window.addEventListener('offline', () => {
    console.log('[DukeCam] Went offline');
    showToast('Offline — photos will upload when connected', 'error');
});

// ─── Camera return detection ─────────────────────────────────────
// Some Android browsers don't fire onchange after camera capture.
// We detect the return via visibilitychange and check for new files.

let cameraInputPending = null;

function watchCameraInput(inputEl) {
    cameraInputPending = inputEl;
    console.log('[DukeCam] Watching for camera return on:', inputEl.id);
}

document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible' && cameraInputPending) {
        console.log('[DukeCam] Page became visible, checking camera input');
        // Small delay — browser needs time to populate the input
        setTimeout(() => {
            const input = cameraInputPending;
            if (input && input.files && input.files.length > 0) {
                console.log(`[DukeCam] visibilitychange: found ${input.files.length} files in ${input.id}`);
                // Only fire if handleFiles exists (set by project page)
                if (typeof handleFiles === 'function') {
                    handleFiles(input.files);
                }
            } else {
                console.log('[DukeCam] visibilitychange: no files found in camera input');
            }
            cameraInputPending = null;
        }, 500);
    }
});

// ─── UI Helpers ──────────────────────────────────────────────────

function renderQueueItem(item) {
    const container = document.getElementById('upload-queue');
    if (!container) return;

    // Show the queue section
    container.style.display = 'block';

    // Don't duplicate
    if (document.getElementById(`queue-${item.id}`)) return;

    const div = document.createElement('div');
    div.id = `queue-${item.id}`;
    div.className = 'queue-item';
    div.innerHTML = `
        <img src="" alt="" />
        <div class="info">
            <div class="name">${escapeHtml(item.fileName || 'Photo')}</div>
            <div class="status">Queued...</div>
            <div class="progress-bar"><div class="fill" style="width: 0%"></div></div>
        </div>
        <button class="retry-btn" onclick="retryItem('${item.id}')">Retry</button>
    `;

    // Create thumbnail preview from blob (Blob/File or legacy ArrayBuffer)
    try {
        const previewBlob = (item.blob instanceof Blob)
            ? item.blob
            : new Blob([item.blob], { type: item.mimeType || 'image/jpeg' });
        const url = URL.createObjectURL(previewBlob);
        div.querySelector('img').src = url;
    } catch (e) {
        console.warn('[DukeCam] Could not create preview:', e);
    }

    container.prepend(div);
}

function updateQueueItem(id, status, text) {
    const el = document.getElementById(`queue-${id}`);
    if (!el) return;

    el.className = 'queue-item ' + (status === 'done' ? 'done' : status === 'error' ? 'error' : '');
    const statusEl = el.querySelector('.status');
    if (statusEl) statusEl.textContent = text;

    // Show/hide retry button
    const retryBtn = el.querySelector('.retry-btn');
    if (retryBtn) retryBtn.style.display = (status === 'error') ? 'inline-block' : 'none';
}

function updateQueueProgress(id, pct) {
    const el = document.getElementById(`queue-${id}`);
    if (!el) return;

    const fill = el.querySelector('.fill');
    if (fill) fill.style.width = pct + '%';

    const statusEl = el.querySelector('.status');
    if (statusEl) statusEl.textContent = `Uploading ${pct}%`;
}

function fadeOutQueueItem(id) {
    setTimeout(() => {
        const el = document.getElementById(`queue-${id}`);
        if (el) {
            el.style.transition = 'opacity 0.5s, max-height 0.5s';
            el.style.opacity = '0';
            el.style.maxHeight = '0';
            el.style.overflow = 'hidden';
            setTimeout(() => el.remove(), 600);
        }
    }, 3000);
}

async function retryItem(id) {
    if (!dbAvailable) return;
    try {
        const tx = db.transaction(STORE_NAME, 'readwrite');
        const store = tx.objectStore(STORE_NAME);
        const req = store.get(id);
        req.onsuccess = async () => {
            const item = req.result;
            if (item) {
                item.status = 'pending';
                item.retries = 0;
                await dbPut(item);
                updateQueueItem(id, '', 'Retrying...');
                processQueue();
            }
        };
    } catch (err) {
        console.error('[DukeCam] retryItem error:', err);
    }
}

function showToast(message, type) {
    console.log(`[DukeCam] Toast: ${message} (${type})`);
    let container = document.getElementById('toast-container');
    if (!container) {
        container = document.createElement('div');
        container.id = 'toast-container';
        container.className = 'toast-container';
        document.body.appendChild(container);
    }

    const toast = document.createElement('div');
    toast.className = 'toast ' + (type || '');
    toast.textContent = message;
    container.appendChild(toast);

    // Animate in
    requestAnimationFrame(() => toast.classList.add('show'));

    setTimeout(() => {
        toast.classList.remove('show');
        setTimeout(() => toast.remove(), 300);
    }, 3500);
}

function escapeHtml(str) {
    const div = document.createElement('div');
    div.textContent = str || '';
    return div.innerHTML;
}

// ─── Initialize ──────────────────────────────────────────────────

console.log('[DukeCam] Upload engine v2 loading');
openDB().then(() => {
    console.log(`[DukeCam] DB ready (available=${dbAvailable})`);
    resumePendingUploads();
}).catch((err) => {
    console.warn('[DukeCam] DB init failed, direct upload mode:', err);
});
