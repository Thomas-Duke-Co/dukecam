/**
 * DukeCam Inspection Offline Storage Layer
 *
 * IndexedDB-backed persistence for inspection data and photo attachments.
 * Ensures inspection progress is never lost due to connectivity gaps.
 *
 * Stores:
 *   - inspection_drafts: Full inspection state (checklist responses, notes, metadata)
 *   - inspection_photos: Pending photo attachments queued for upload
 *
 * Integrates with the existing DukeCam IndexedDB (bumps version to 3).
 */

const INSPECTION_DB_NAME = 'dukecam';
const INSPECTION_DB_VERSION = 3;
const DRAFT_STORE = 'inspection_drafts';
const PHOTO_QUEUE_STORE = 'inspection_photos';

let inspectionDB = null;
let inspectionDBReady = false;

// ─── IndexedDB Setup ─────────────────────────────────────────────

/**
 * Opens (or upgrades) the IndexedDB with inspection stores.
 * Preserves the existing upload_queue store from upload.js.
 */
async function openInspectionDB() {
    if (inspectionDB && inspectionDBReady) return inspectionDB;

    try {
        return await new Promise((resolve, reject) => {
            const req = indexedDB.open(INSPECTION_DB_NAME, INSPECTION_DB_VERSION);

            req.onupgradeneeded = (e) => {
                const database = e.target.result;

                // Preserve existing upload_queue store (from upload.js v2)
                if (!database.objectStoreNames.contains('upload_queue')) {
                    const store = database.createObjectStore('upload_queue', { keyPath: 'id' });
                    store.createIndex('status', 'status', { unique: false });
                }

                // Create inspection drafts store
                if (!database.objectStoreNames.contains(DRAFT_STORE)) {
                    const draftStore = database.createObjectStore(DRAFT_STORE, { keyPath: 'localId' });
                    draftStore.createIndex('serverId', 'serverId', { unique: false });
                    draftStore.createIndex('status', 'status', { unique: false });
                    draftStore.createIndex('syncStatus', 'syncStatus', { unique: false });
                    draftStore.createIndex('updatedAt', 'updatedAt', { unique: false });
                }

                // Create inspection photo queue store
                if (!database.objectStoreNames.contains(PHOTO_QUEUE_STORE)) {
                    const photoStore = database.createObjectStore(PHOTO_QUEUE_STORE, { keyPath: 'id' });
                    photoStore.createIndex('inspectionLocalId', 'inspectionLocalId', { unique: false });
                    photoStore.createIndex('syncStatus', 'syncStatus', { unique: false });
                    photoStore.createIndex('itemKey', 'itemKey', { unique: false });
                    photoStore.createIndex('queuedAt', 'queuedAt', { unique: false });
                }
            };

            req.onsuccess = (e) => {
                inspectionDB = e.target.result;
                inspectionDBReady = true;
                console.log('[Inspection] IndexedDB ready (v3)');
                resolve(inspectionDB);
            };

            req.onerror = (e) => {
                console.error('[Inspection] IndexedDB open failed:', e.target.error);
                reject(e.target.error);
            };
        });
    } catch (err) {
        console.error('[Inspection] IndexedDB unavailable:', err);
        inspectionDBReady = false;
        return null;
    }
}

// ─── Generic IDB Helpers ─────────────────────────────────────────

function idbPut(storeName, item) {
    if (!inspectionDBReady) return Promise.reject(new Error('DB not ready'));
    return new Promise((resolve, reject) => {
        try {
            const tx = inspectionDB.transaction(storeName, 'readwrite');
            const store = tx.objectStore(storeName);
            const req = store.put(item);
            tx.oncomplete = () => resolve(req.result);
            tx.onerror = (e) => reject(e.target.error);
        } catch (err) {
            reject(err);
        }
    });
}

function idbGet(storeName, key) {
    if (!inspectionDBReady) return Promise.resolve(null);
    return new Promise((resolve, reject) => {
        try {
            const tx = inspectionDB.transaction(storeName, 'readonly');
            const req = tx.objectStore(storeName).get(key);
            req.onsuccess = () => resolve(req.result || null);
            req.onerror = (e) => reject(e.target.error);
        } catch (err) {
            reject(err);
        }
    });
}

function idbDelete(storeName, key) {
    if (!inspectionDBReady) return Promise.resolve();
    return new Promise((resolve, reject) => {
        try {
            const tx = inspectionDB.transaction(storeName, 'readwrite');
            tx.objectStore(storeName).delete(key);
            tx.oncomplete = () => resolve();
            tx.onerror = (e) => reject(e.target.error);
        } catch (err) {
            resolve(); // Don't block on cleanup failure
        }
    });
}

function idbGetAll(storeName) {
    if (!inspectionDBReady) return Promise.resolve([]);
    return new Promise((resolve, reject) => {
        try {
            const tx = inspectionDB.transaction(storeName, 'readonly');
            const req = tx.objectStore(storeName).getAll();
            req.onsuccess = () => resolve(req.result || []);
            req.onerror = (e) => reject(e.target.error);
        } catch (err) {
            resolve([]);
        }
    });
}

function idbGetByIndex(storeName, indexName, value) {
    if (!inspectionDBReady) return Promise.resolve([]);
    return new Promise((resolve, reject) => {
        try {
            const tx = inspectionDB.transaction(storeName, 'readonly');
            const store = tx.objectStore(storeName);
            const idx = store.index(indexName);
            const req = idx.getAll(value);
            req.onsuccess = () => resolve(req.result || []);
            req.onerror = (e) => reject(e.target.error);
        } catch (err) {
            resolve([]);
        }
    });
}

function idbCount(storeName) {
    if (!inspectionDBReady) return Promise.resolve(0);
    return new Promise((resolve, reject) => {
        try {
            const tx = inspectionDB.transaction(storeName, 'readonly');
            const req = tx.objectStore(storeName).count();
            req.onsuccess = () => resolve(req.result);
            req.onerror = (e) => reject(e.target.error);
        } catch (err) {
            resolve(0);
        }
    });
}

function idbClear(storeName) {
    if (!inspectionDBReady) return Promise.resolve();
    return new Promise((resolve, reject) => {
        try {
            const tx = inspectionDB.transaction(storeName, 'readwrite');
            tx.objectStore(storeName).clear();
            tx.oncomplete = () => resolve();
            tx.onerror = (e) => reject(e.target.error);
        } catch (err) {
            resolve();
        }
    });
}

// ─── Inspection Draft CRUD ───────────────────────────────────────

/**
 * Generate a unique local ID for drafts.
 */
function generateLocalId() {
    return 'insp_' + Date.now() + '_' + Math.random().toString(36).slice(2, 10);
}

/**
 * Create a new inspection draft in IndexedDB.
 *
 * @param {Object} params
 * @param {number} params.serverId - Server inspection ID (if already created server-side)
 * @param {number} [params.templateId] - Template ID used
 * @param {string} [params.templateName] - Template name for display
 * @param {number} params.propertyId - PropertyOS property ID
 * @param {string} params.propertyName - Property name for display
 * @param {number} [params.buildingId] - Building ID
 * @param {number} [params.unitId] - Unit/suite ID
 * @param {string} [params.unitName] - Unit name for display
 * @param {string} [params.inspectorId] - Fyxt inspector ID
 * @param {string} params.inspectorName - Inspector name
 * @param {Array} [params.checklist] - Array of checklist items with responses
 * @returns {Promise<Object>} The saved draft
 */
async function createInspectionDraft(params) {
    const draft = {
        localId: generateLocalId(),
        serverId: params.serverId || null,
        templateId: params.templateId || null,
        templateName: params.templateName || null,
        propertyId: params.propertyId,
        propertyName: params.propertyName || '',
        buildingId: params.buildingId || null,
        unitId: params.unitId || null,
        unitName: params.unitName || null,
        inspectorId: params.inspectorId || null,
        inspectorName: params.inspectorName || '',
        status: 'in_progress', // draft, in_progress, completed
        syncStatus: 'synced',  // synced, pending, error
        notes: null,
        // Checklist responses keyed by item identifier
        // Format: { "item_<id>": { status: "pass"|"fail"|"needs_attention"|null, notes: "" },
        //           "adhoc_<id>": { status: ..., notes: "", label: "" } }
        responses: params.checklist || {},
        // Adhoc items added during inspection
        adhocItems: [],
        createdAt: Date.now(),
        updatedAt: Date.now(),
        completedAt: null,
    };

    await idbPut(DRAFT_STORE, draft);
    console.log(`[Inspection] Draft created: ${draft.localId} (server=${draft.serverId})`);
    return draft;
}

/**
 * Get an inspection draft by local ID.
 */
async function getInspectionDraft(localId) {
    return await idbGet(DRAFT_STORE, localId);
}

/**
 * Get an inspection draft by its server-side ID.
 */
async function getInspectionDraftByServerId(serverId) {
    const results = await idbGetByIndex(DRAFT_STORE, 'serverId', serverId);
    return results.length > 0 ? results[0] : null;
}

/**
 * Update a checklist item response within a draft.
 *
 * @param {string} localId - Draft local ID
 * @param {string} itemKey - Item key (e.g. "item_42" or "adhoc_5")
 * @param {Object} response - { status: "pass"|"fail"|"needs_attention"|null, notes: "" }
 * @returns {Promise<Object>} Updated draft
 */
async function updateDraftResponse(localId, itemKey, response) {
    const draft = await getInspectionDraft(localId);
    if (!draft) throw new Error(`Draft not found: ${localId}`);

    draft.responses[itemKey] = {
        ...draft.responses[itemKey],
        ...response,
        updatedAt: Date.now(),
    };
    draft.updatedAt = Date.now();
    draft.syncStatus = 'pending';

    await idbPut(DRAFT_STORE, draft);
    return draft;
}

/**
 * Add an ad-hoc checklist item to a draft.
 *
 * @param {string} localId - Draft local ID
 * @param {Object} item - { label, categoryName, notes }
 * @returns {Promise<Object>} Updated draft with new adhoc item
 */
async function addDraftAdhocItem(localId, item) {
    const draft = await getInspectionDraft(localId);
    if (!draft) throw new Error(`Draft not found: ${localId}`);

    const adhocId = 'adhoc_local_' + Date.now() + '_' + Math.random().toString(36).slice(2, 6);
    const adhocItem = {
        localId: adhocId,
        serverId: null,
        label: item.label,
        categoryName: item.categoryName || 'Ad-hoc Items',
        notes: item.notes || null,
        status: null,
        createdAt: Date.now(),
    };

    draft.adhocItems.push(adhocItem);
    draft.responses[adhocId] = { status: null, notes: item.notes || '' };
    draft.updatedAt = Date.now();
    draft.syncStatus = 'pending';

    await idbPut(DRAFT_STORE, draft);
    console.log(`[Inspection] Adhoc item added to ${localId}: ${adhocId}`);
    return draft;
}

/**
 * Update draft metadata (notes, status, etc).
 */
async function updateInspectionDraft(localId, updates) {
    const draft = await getInspectionDraft(localId);
    if (!draft) throw new Error(`Draft not found: ${localId}`);

    Object.assign(draft, updates);
    draft.updatedAt = Date.now();
    if (updates.status === 'completed' && !draft.completedAt) {
        draft.completedAt = Date.now();
    }

    await idbPut(DRAFT_STORE, draft);
    return draft;
}

/**
 * Mark a draft as synced with the server.
 */
async function markDraftSynced(localId, serverId) {
    const draft = await getInspectionDraft(localId);
    if (!draft) return;

    draft.serverId = serverId || draft.serverId;
    draft.syncStatus = 'synced';
    draft.updatedAt = Date.now();

    await idbPut(DRAFT_STORE, draft);
    return draft;
}

/**
 * Delete a draft and all its associated pending photos.
 */
async function deleteInspectionDraft(localId) {
    // Delete associated pending photos first
    const photos = await getPhotosForInspection(localId);
    for (const photo of photos) {
        await idbDelete(PHOTO_QUEUE_STORE, photo.id);
    }

    await idbDelete(DRAFT_STORE, localId);
    console.log(`[Inspection] Draft deleted: ${localId} (${photos.length} photos removed)`);
}

/**
 * List all inspection drafts, optionally filtered by sync status.
 */
async function listInspectionDrafts(syncStatus) {
    if (syncStatus) {
        return await idbGetByIndex(DRAFT_STORE, 'syncStatus', syncStatus);
    }
    return await idbGetAll(DRAFT_STORE);
}

/**
 * Get count of drafts with pending changes.
 */
async function getPendingSyncCount() {
    const pending = await idbGetByIndex(DRAFT_STORE, 'syncStatus', 'pending');
    const pendingPhotos = await idbGetByIndex(PHOTO_QUEUE_STORE, 'syncStatus', 'pending');
    return { drafts: pending.length, photos: pendingPhotos.length };
}

// ─── Inspection Photo Queue CRUD ─────────────────────────────────

/**
 * Generate a unique photo queue ID.
 */
function generatePhotoId() {
    return 'photo_' + Date.now() + '_' + Math.random().toString(36).slice(2, 10);
}

/**
 * Queue a photo for upload, associated with an inspection and optionally a checklist item.
 *
 * @param {Object} params
 * @param {string} params.inspectionLocalId - Local draft ID
 * @param {number} [params.inspectionServerId] - Server inspection ID
 * @param {string} [params.itemKey] - Checklist item key ("item_42" or "adhoc_5")
 * @param {number} [params.itemId] - Server-side item ID
 * @param {number} [params.adhocItemId] - Server-side adhoc item ID
 * @param {File|Blob} params.file - The photo file
 * @param {string} [params.caption] - Photo caption
 * @param {number} [params.lat] - GPS latitude
 * @param {number} [params.lng] - GPS longitude
 * @returns {Promise<Object>} The queued photo record
 */
async function queueInspectionPhoto(params) {
    const file = params.file;
    const buffer = await file.arrayBuffer();

    const photo = {
        id: generatePhotoId(),
        inspectionLocalId: params.inspectionLocalId,
        inspectionServerId: params.inspectionServerId || null,
        itemKey: params.itemKey || null,
        itemId: params.itemId || null,
        adhocItemId: params.adhocItemId || null,
        blob: buffer,
        fileName: file.name || `inspection-${Date.now()}.jpg`,
        mimeType: file.type || 'image/jpeg',
        fileSize: file.size || buffer.byteLength,
        caption: params.caption || null,
        lat: params.lat || null,
        lng: params.lng || null,
        syncStatus: 'pending',  // pending, uploading, synced, error
        retries: 0,
        maxRetries: 10,
        errorMessage: null,
        queuedAt: Date.now(),
        uploadedAt: null,
        serverPhotoId: null,
    };

    await idbPut(PHOTO_QUEUE_STORE, photo);
    console.log(`[Inspection] Photo queued: ${photo.id} for inspection ${photo.inspectionLocalId}, item=${photo.itemKey}`);
    return photo;
}

/**
 * Get a single queued photo by ID.
 */
async function getQueuedPhoto(id) {
    return await idbGet(PHOTO_QUEUE_STORE, id);
}

/**
 * Get all photos queued for a specific inspection.
 */
async function getPhotosForInspection(inspectionLocalId) {
    return await idbGetByIndex(PHOTO_QUEUE_STORE, 'inspectionLocalId', inspectionLocalId);
}

/**
 * Get all photos for a specific checklist item within an inspection.
 */
async function getPhotosForItem(itemKey) {
    return await idbGetByIndex(PHOTO_QUEUE_STORE, 'itemKey', itemKey);
}

/**
 * Get all photos pending upload.
 */
async function getPendingPhotos() {
    return await idbGetByIndex(PHOTO_QUEUE_STORE, 'syncStatus', 'pending');
}

/**
 * Get all photos that failed upload.
 */
async function getFailedPhotos() {
    return await idbGetByIndex(PHOTO_QUEUE_STORE, 'syncStatus', 'error');
}

/**
 * Mark a photo as currently uploading.
 */
async function markPhotoUploading(id) {
    const photo = await getQueuedPhoto(id);
    if (!photo) return null;

    photo.syncStatus = 'uploading';
    await idbPut(PHOTO_QUEUE_STORE, photo);
    return photo;
}

/**
 * Mark a photo as successfully uploaded.
 */
async function markPhotoSynced(id, serverPhotoId) {
    const photo = await getQueuedPhoto(id);
    if (!photo) return null;

    photo.syncStatus = 'synced';
    photo.uploadedAt = Date.now();
    photo.serverPhotoId = serverPhotoId || null;
    // Clear blob to free memory (keep metadata for reference)
    photo.blob = null;

    await idbPut(PHOTO_QUEUE_STORE, photo);
    return photo;
}

/**
 * Mark a photo upload as failed with an error message.
 */
async function markPhotoError(id, errorMessage) {
    const photo = await getQueuedPhoto(id);
    if (!photo) return null;

    photo.retries++;
    if (photo.retries >= photo.maxRetries) {
        photo.syncStatus = 'error';
        photo.errorMessage = errorMessage || 'Max retries exceeded';
    } else {
        // Back to pending for retry
        photo.syncStatus = 'pending';
        photo.errorMessage = errorMessage;
    }

    await idbPut(PHOTO_QUEUE_STORE, photo);
    return photo;
}

/**
 * Reset a failed photo back to pending for retry.
 */
async function retryPhoto(id) {
    const photo = await getQueuedPhoto(id);
    if (!photo) return null;

    photo.syncStatus = 'pending';
    photo.retries = 0;
    photo.errorMessage = null;

    await idbPut(PHOTO_QUEUE_STORE, photo);
    console.log(`[Inspection] Photo retry queued: ${id}`);
    return photo;
}

/**
 * Delete a queued photo (e.g., user removes before upload).
 */
async function deleteQueuedPhoto(id) {
    await idbDelete(PHOTO_QUEUE_STORE, id);
    console.log(`[Inspection] Photo deleted from queue: ${id}`);
}

/**
 * Remove all synced photos (cleanup after successful uploads).
 */
async function cleanupSyncedPhotos() {
    const all = await idbGetAll(PHOTO_QUEUE_STORE);
    let cleaned = 0;
    for (const photo of all) {
        if (photo.syncStatus === 'synced') {
            await idbDelete(PHOTO_QUEUE_STORE, photo.id);
            cleaned++;
        }
    }
    if (cleaned > 0) {
        console.log(`[Inspection] Cleaned up ${cleaned} synced photos`);
    }
    return cleaned;
}

// ─── Sync Queue Management ───────────────────────────────────────

/**
 * Get the full sync queue status for display.
 * Returns counts and items needing attention.
 */
async function getSyncQueueStatus() {
    const drafts = await idbGetAll(DRAFT_STORE);
    const photos = await idbGetAll(PHOTO_QUEUE_STORE);

    const pendingDrafts = drafts.filter(d => d.syncStatus === 'pending');
    const errorDrafts = drafts.filter(d => d.syncStatus === 'error');
    const pendingPhotos = photos.filter(p => p.syncStatus === 'pending');
    const uploadingPhotos = photos.filter(p => p.syncStatus === 'uploading');
    const errorPhotos = photos.filter(p => p.syncStatus === 'error');

    return {
        totalDrafts: drafts.length,
        pendingDrafts: pendingDrafts.length,
        errorDrafts: errorDrafts.length,
        totalPhotos: photos.length,
        pendingPhotos: pendingPhotos.length,
        uploadingPhotos: uploadingPhotos.length,
        errorPhotos: errorPhotos.length,
        hasWork: pendingDrafts.length > 0 || pendingPhotos.length > 0,
        hasErrors: errorDrafts.length > 0 || errorPhotos.length > 0,
    };
}

/**
 * Get the next batch of items to sync (drafts first, then photos).
 * Returns { drafts: [...], photos: [...] }.
 */
async function getNextSyncBatch(batchSize) {
    batchSize = batchSize || 5;

    const pendingDrafts = await idbGetByIndex(DRAFT_STORE, 'syncStatus', 'pending');
    const pendingPhotos = await idbGetByIndex(PHOTO_QUEUE_STORE, 'syncStatus', 'pending');

    // Sort by oldest first
    pendingDrafts.sort((a, b) => a.updatedAt - b.updatedAt);
    pendingPhotos.sort((a, b) => a.queuedAt - b.queuedAt);

    return {
        drafts: pendingDrafts.slice(0, batchSize),
        photos: pendingPhotos.slice(0, batchSize),
    };
}

/**
 * Reset all "uploading" items back to "pending" (e.g., after page reload).
 * Prevents items from getting stuck in uploading state.
 */
async function resetStuckUploads() {
    const photos = await idbGetAll(PHOTO_QUEUE_STORE);
    let reset = 0;
    for (const photo of photos) {
        if (photo.syncStatus === 'uploading') {
            photo.syncStatus = 'pending';
            await idbPut(PHOTO_QUEUE_STORE, photo);
            reset++;
        }
    }
    if (reset > 0) {
        console.log(`[Inspection] Reset ${reset} stuck uploads to pending`);
    }
    return reset;
}

// ─── Thumbnail Helper ────────────────────────────────────────────

/**
 * Create a data URL thumbnail from a queued photo's blob.
 * Used for offline preview display.
 */
function createPhotoThumbnail(photo) {
    if (!photo || !photo.blob) return null;
    try {
        const blob = new Blob([photo.blob], { type: photo.mimeType || 'image/jpeg' });
        return URL.createObjectURL(blob);
    } catch (e) {
        console.warn('[Inspection] Could not create thumbnail:', e);
        return null;
    }
}

/**
 * Revoke a previously created thumbnail URL to free memory.
 */
function revokePhotoThumbnail(url) {
    if (url && url.startsWith('blob:')) {
        URL.revokeObjectURL(url);
    }
}

// ─── Initialize ──────────────────────────────────────────────────

/**
 * Initialize the inspection offline storage.
 * Should be called on page load for inspection pages.
 */
async function initInspectionOffline() {
    try {
        await openInspectionDB();
        if (inspectionDBReady) {
            await resetStuckUploads();
            await cleanupSyncedPhotos();
            const status = await getSyncQueueStatus();
            if (status.hasWork) {
                console.log(`[Inspection] Pending sync: ${status.pendingDrafts} drafts, ${status.pendingPhotos} photos`);
            }
            if (status.hasErrors) {
                console.log(`[Inspection] Sync errors: ${status.errorDrafts} drafts, ${status.errorPhotos} photos`);
            }
        }
    } catch (err) {
        console.error('[Inspection] Offline init failed:', err);
    }
}

// Auto-initialize when loaded on inspection pages
if (typeof window !== 'undefined') {
    // Don't auto-init on project pages — only when explicitly needed
    console.log('[Inspection] Offline storage module loaded');
}
