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
const DB_VERSION = 2;
const STORE_NAME = 'upload_queue';
const MAX_CONCURRENT = 2;
const MAX_RETRIES = 10;
const RETRY_BASE_MS = 2000;

let db = null;
let dbAvailable = false;
let activeUploads = 0;
let isProcessing = false;

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
    console.log(`[DukeCam] enqueueFiles: ${files.length} files, project=${projectId}, worker=${workerId || workerName || 'none'}, tag=${tag}`);
    const batchId = (crypto.randomUUID ? crypto.randomUUID() : Date.now().toString());

    for (const file of files) {
        const sizeMB = (file.size / 1024 / 1024).toFixed(1);
        console.log(`[DukeCam] Processing: ${file.name} (${sizeMB}MB, ${file.type || 'unknown type'})`);

        try {
            const buffer = await file.arrayBuffer();
            console.log(`[DukeCam] Read ${buffer.byteLength} bytes from ${file.name}`);

            const item = {
                id: `${batchId}-${Date.now()}-${Math.random().toString(36).slice(2, 8)}`,
                blob: buffer,
                fileName: file.name || `photo-${Date.now()}.jpg`,
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

            if (dbAvailable) {
                await dbPut(item);
                console.log(`[DukeCam] Queued in IndexedDB: ${item.id}`);
                renderQueueItem(item);
            } else {
                // Fallback: direct upload without queue
                console.log(`[DukeCam] Direct upload (no IndexedDB): ${item.fileName}`);
                renderQueueItem(item);
                directUpload(item);
            }
        } catch (err) {
            console.error(`[DukeCam] Failed to read file ${file.name}:`, err);
            showToast(`Failed to read ${file.name}`, 'error');
        }
    }

    if (dbAvailable) {
        console.log(`[DukeCam] All files queued, starting processQueue`);
        processQueue();
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
        showToast('Photo uploaded!', 'success');
        fadeOutQueueItem(item.id);

    } catch (err) {
        console.error(`[DukeCam] Upload failed: ${item.fileName}`, err);
        item.retries++;
        if (item.retries >= MAX_RETRIES) {
            item.status = 'failed';
            await dbPut(item);
            updateQueueItem(item.id, 'error', `Failed after ${MAX_RETRIES} attempts`);
            showToast('Upload failed — tap to retry', 'error');
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
            showToast('Photo uploaded!', 'success');
            fadeOutQueueItem(item.id);
            return;
        } catch (err) {
            retries++;
            console.error(`[DukeCam] Direct upload attempt ${retries} failed:`, err);
            if (retries >= MAX_RETRIES) {
                updateQueueItem(item.id, 'error', `Failed after ${MAX_RETRIES} attempts`);
                showToast('Upload failed', 'error');
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
    const blob = new Blob([item.blob], { type: item.mimeType || 'image/jpeg' });
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

    // Create thumbnail preview from blob
    try {
        const blob = new Blob([item.blob], { type: item.mimeType || 'image/jpeg' });
        const url = URL.createObjectURL(blob);
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
