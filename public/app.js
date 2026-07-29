/* Markdown editor / organizer — front end.
 *
 * No framework and no bundler: the page is served as-is, which keeps the strict
 * Content-Security-Policy (no inline script, no inline style) workable.
 *
 * The GitHub token lives in an encrypted HTTP-only cookie, so nothing here ever
 * sees it. Every mutating call carries the CSRF token handed out by /api/me. */

'use strict';

/* ── Small DOM helpers ───────────────────────────────────────────────────── */

const $ = (id) => document.getElementById(id);
const create = (tag, className, text) => {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
};
const icon = (name, className) => {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('viewBox', '0 0 16 16');
  svg.setAttribute('aria-hidden', 'true');
  svg.classList.add('icon');
  if (className) svg.classList.add(...className.split(' '));
  const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
  use.setAttribute('href', `#${name}`);
  svg.appendChild(use);
  return svg;
};

/* ── Persistent state ────────────────────────────────────────────────────── */

const STORE_KEY = 'mde.v1';
const DRAFT_PREFIX = 'mde.draft.';

const defaultSettings = {
  view: 'split',
  fontSize: 14,
  syncScroll: true,
  drafts: true,
  sidebarHidden: false,
  editorFraction: 1,
};

const emptyStore = () => ({
  repos: [], activeRepo: null, settings: { ...defaultSettings }, expanded: [], bookmarks: [],
});

function loadStore() {
  try {
    const raw = localStorage.getItem(STORE_KEY);
    if (!raw) return emptyStore();
    const parsed = JSON.parse(raw);
    return {
      repos: Array.isArray(parsed.repos) ? parsed.repos : [],
      activeRepo: parsed.activeRepo || null,
      settings: { ...defaultSettings, ...(parsed.settings || {}) },
      expanded: Array.isArray(parsed.expanded) ? parsed.expanded : [],
      bookmarks: Array.isArray(parsed.bookmarks) ? parsed.bookmarks : [],
    };
  } catch {
    return emptyStore();
  }
}

function saveStore() {
  try {
    localStorage.setItem(STORE_KEY, JSON.stringify({
      repos: state.repos,
      activeRepo: state.activeRepo,
      settings: state.settings,
      expanded: [...state.expanded],
      bookmarks: state.bookmarks,
    }));
  } catch { /* storage full or disabled — the app still works, just forgets */ }
}

/* ── In-memory state ─────────────────────────────────────────────────────── */

const persisted = loadStore();

const state = {
  user: null,
  csrf: null,
  repos: persisted.repos,          // connected repositories
  activeRepo: persisted.activeRepo, // full_name of the selected repository
  settings: persisted.settings,
  expanded: new Set(persisted.expanded),
  bookmarks: persisted.bookmarks,     // pinned documents, across repositories
  branches: [],
  tree: [],
  treeTruncated: false,
  docs: new Map(),                 // key -> document
  activeKey: null,
  formats: [],
  filter: '',
  localSeq: 0,
};

const repoOf = (fullName) => state.repos.find((r) => r.fullName === fullName) || null;
const activeRepo = () => (state.activeRepo ? repoOf(state.activeRepo) : null);
const activeDoc = () => (state.activeKey ? state.docs.get(state.activeKey) || null : null);
const isDirty = (doc) => !!doc && (doc.isNew || doc.content !== doc.baseContent);

/* ── API layer ───────────────────────────────────────────────────────────── */

class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

async function api(path, { method = 'GET', body, raw = false } = {}) {
  const headers = {};
  const options = { method, headers, credentials: 'same-origin' };

  if (body !== undefined) {
    headers['Content-Type'] = 'application/json';
    options.body = JSON.stringify(body);
  }
  if (method !== 'GET' && method !== 'HEAD') {
    if (!state.csrf) throw new ApiError(403, 'Session not ready — reload the page.');
    headers['X-CSRF-Token'] = state.csrf;
  }

  const response = await fetch(path, options);

  if (response.status === 401) {
    // The session expired or the token was revoked upstream.
    redirectToLogin();
    throw new ApiError(401, 'Your session expired. Redirecting to sign in…');
  }
  if (!response.ok) {
    let message = `Request failed (${response.status})`;
    try {
      const data = await response.json();
      if (data && data.error) message = data.error;
    } catch { /* non-JSON error body */ }
    throw new ApiError(response.status, message);
  }
  if (raw) return response;
  if (response.status === 204) return null;
  return response.json();
}

function redirectToLogin() {
  const target = `/login.html?redirect=${encodeURIComponent(location.pathname)}`;
  location.replace(target);
}

const repoPath = (repo, suffix = '') =>
  `/api/repos/${encodeURIComponent(repo.owner)}/${encodeURIComponent(repo.name)}${suffix}`;

/* ── Toasts ──────────────────────────────────────────────────────────────── */

function toast(message, kind = 'info', { timeout = 5200, link } = {}) {
  const node = create('div', `toast ${kind}`);
  const body = create('div');
  body.style.flex = '1 1 auto';
  body.textContent = message;
  if (link) {
    body.appendChild(document.createTextNode(' '));
    const anchor = create('a', null, link.label);
    anchor.href = link.href;
    anchor.target = '_blank';
    anchor.rel = 'noopener noreferrer';
    body.appendChild(anchor);
  }
  node.appendChild(body);

  const close = create('button', 'toast-close', '×');
  close.type = 'button';
  close.setAttribute('aria-label', 'Dismiss');
  close.addEventListener('click', () => node.remove());
  node.appendChild(close);

  $('toasts').appendChild(node);
  if (timeout) setTimeout(() => node.remove(), timeout);
  return node;
}

const failToast = (err) => toast(err instanceof Error ? err.message : String(err), 'error', { timeout: 8000 });

function busy(button, on) {
  if (!button) return;
  button.classList.toggle('is-busy', on);
  button.disabled = on;
}

/* ── Dialogs ─────────────────────────────────────────────────────────────── */

let openDialog = null;
let lastFocused = null;

function showDialog(id) {
  closeDialog();
  lastFocused = document.activeElement;
  openDialog = $(id);
  $('overlay').hidden = false;
  openDialog.hidden = false;
  const focusable = openDialog.querySelector('input, select, button:not([data-close-dialog])');
  if (focusable) focusable.focus();
}

function closeDialog() {
  if (!openDialog) return;
  openDialog.hidden = true;
  openDialog = null;
  $('overlay').hidden = true;
  if (lastFocused && document.contains(lastFocused)) lastFocused.focus();
}

/** promptDialog asks for a single line of text, resolving to null on cancel. */
function promptDialog({ title, hint = '', value = '', confirmLabel = 'OK', validate }) {
  return new Promise((resolve) => {
    $('prompt-title').textContent = title;
    $('prompt-hint').textContent = hint;
    $('prompt-hint').hidden = !hint;
    $('prompt-confirm').textContent = confirmLabel;
    const input = $('prompt-input');
    const error = $('prompt-error');
    input.value = value;
    error.hidden = true;

    const form = $('prompt-form');
    const cancels = [...$('dialog-prompt').querySelectorAll('[data-close-dialog]')];

    const cleanup = () => {
      form.removeEventListener('submit', onSubmit);
      cancels.forEach((b) => b.removeEventListener('click', onCancel));
      document.removeEventListener('mde:dialog-dismissed', onCancel);
    };
    const finish = (result) => {
      cleanup();
      closeDialog();
      resolve(result);
    };
    const onCancel = () => finish(null);
    const onSubmit = (event) => {
      event.preventDefault();
      const raw = input.value.trim();
      const problem = validate ? validate(raw) : (raw ? null : 'Please enter a value.');
      if (problem) {
        error.textContent = problem;
        error.hidden = false;
        input.focus();
        return;
      }
      finish(raw);
    };

    form.addEventListener('submit', onSubmit);
    cancels.forEach((b) => b.addEventListener('click', onCancel));
    document.addEventListener('mde:dialog-dismissed', onCancel);

    showDialog('dialog-prompt');
    input.select();
  });
}

/** confirmDialog resolves true only when the destructive action is confirmed. */
function confirmDialog({ title, body, confirmLabel = 'Delete' }) {
  return new Promise((resolve) => {
    $('confirm-title').textContent = title;
    $('confirm-body').textContent = body;
    const ok = $('confirm-ok');
    ok.textContent = confirmLabel;
    const cancels = [...$('dialog-confirm').querySelectorAll('[data-close-dialog]')];

    const cleanup = () => {
      ok.removeEventListener('click', onOk);
      cancels.forEach((b) => b.removeEventListener('click', onCancel));
      document.removeEventListener('mde:dialog-dismissed', onCancel);
    };
    const finish = (result) => {
      cleanup();
      closeDialog();
      resolve(result);
    };
    const onOk = () => finish(true);
    const onCancel = () => finish(false);

    ok.addEventListener('click', onOk);
    cancels.forEach((b) => b.addEventListener('click', onCancel));
    document.addEventListener('mde:dialog-dismissed', onCancel);

    showDialog('dialog-confirm');
    ok.focus();
  });
}

/* ── Paths ───────────────────────────────────────────────────────────────── */

const MARKDOWN_EXT = /\.(md|markdown|mdown|mkd|mdx|text)$/i;

function normalizePath(input) {
  const cleaned = String(input || '')
    .replace(/\\/g, '/')
    .split('/')
    .map((part) => part.trim())
    .filter((part) => part && part !== '.')
    .join('/');
  return cleaned;
}

/** validatePath mirrors the server's rules so mistakes are caught before a round trip. */
function validatePath(input, { requireMarkdown = true } = {}) {
  const path = normalizePath(input);
  if (!path) return 'Please enter a file name.';
  if (path.length > 512) return 'That path is too long.';
  if (path.split('/').some((part) => part === '..')) return 'Paths cannot contain "..".';
  if (path.split('/').some((part) => part.toLowerCase() === '.git')) return 'Paths cannot go inside .git.';
  if (/[\u0000-\u001f\u007f]/.test(path)) return 'That path contains invalid characters.';
  if (requireMarkdown && !MARKDOWN_EXT.test(path)) return 'The file name needs a Markdown extension, such as .md.';
  return null;
}

const dirName = (path) => {
  const i = path.lastIndexOf('/');
  return i === -1 ? '' : path.slice(0, i);
};
const baseName = (path) => path.slice(path.lastIndexOf('/') + 1);
const joinPath = (dir, name) => (dir ? `${dir}/${name}` : name);

/* ── Documents ───────────────────────────────────────────────────────────── */

const docKey = (repoFullName, branch, path) =>
  repoFullName ? `repo:${repoFullName}@${branch}:${path}` : `local:${path}`;

function draftKey(doc) { return DRAFT_PREFIX + doc.key; }

function saveDraft(doc) {
  if (!state.settings.drafts) return;
  try {
    if (!isDirty(doc)) {
      localStorage.removeItem(draftKey(doc));
      return;
    }
    localStorage.setItem(draftKey(doc), JSON.stringify({
      content: doc.content,
      baseContent: doc.baseContent,
      sha: doc.sha,
      path: doc.path,
      repo: doc.repoFullName,
      branch: doc.branch,
      isNew: doc.isNew,
      savedAt: Date.now(),
    }));
  } catch { /* ignore quota errors */ }
}

function readDraft(key) {
  try {
    const raw = localStorage.getItem(DRAFT_PREFIX + key);
    return raw ? JSON.parse(raw) : null;
  } catch {
    return null;
  }
}

function dropDraft(doc) {
  // Cancel any queued write for this document first: letting it fire after the
  // draft was dropped would resurrect it right after a successful push.
  if (draftPending && draftPending.key === doc.key) {
    clearTimeout(draftTimer);
    draftPending = null;
  }
  try { localStorage.removeItem(draftKey(doc)); } catch { /* ignore */ }
}

/* Persisting a draft serializes the whole document and writes it synchronously.
 * Doing that on every keystroke is fine for a short note and painful for a large
 * one, so coalesce the writes and flush whenever the page might go away. */
let draftTimer = null;
let draftPending = null;

function queueDraftSave(doc) {
  draftPending = doc;
  clearTimeout(draftTimer);
  draftTimer = setTimeout(flushDraftSave, 600);
}

function flushDraftSave() {
  clearTimeout(draftTimer);
  if (!draftPending) return;
  const doc = draftPending;
  draftPending = null;
  saveDraft(doc);
}

/** restoreDrafts brings saved drafts back into memory on load.
 *
 *  Without this, a document created but not yet pushed would vanish from the
 *  sidebar after a reload even though its text was still sitting in storage. */
function restoreDrafts() {
  if (!state.settings.drafts) return;

  let keys = [];
  try {
    keys = Object.keys(localStorage).filter((key) => key.startsWith(DRAFT_PREFIX));
  } catch {
    return;
  }

  for (const key of keys) {
    let draft;
    try {
      draft = JSON.parse(localStorage.getItem(key));
    } catch {
      continue;
    }
    if (!draft || typeof draft.content !== 'string' || !draft.path) continue;
    // Drop drafts belonging to a repository that is no longer connected.
    if (draft.repo && !repoOf(draft.repo)) continue;

    const doc = {
      key: key.slice(DRAFT_PREFIX.length),
      repoFullName: draft.repo || null,
      branch: draft.branch || '',
      path: draft.path,
      sha: draft.sha || null,
      content: draft.content,
      // A new file has no base; an edited file keeps the text it was based on so
      // the stale-SHA check still protects whatever is on GitHub now.
      baseContent: draft.isNew ? null : (typeof draft.baseContent === 'string' ? draft.baseContent : draft.content),
      isNew: !!draft.isNew,
    };
    state.docs.set(doc.key, doc);
  }
}

function makeDoc({ repoFullName = null, branch = '', path, content = '', sha = null, isNew = false }) {
  return {
    key: docKey(repoFullName, branch, path),
    repoFullName,
    branch,
    path,
    sha,
    content,
    baseContent: isNew ? null : content,
    isNew,
  };
}

/** setActiveDoc swaps the editor over to a document and refreshes the chrome. */
function setActiveDoc(doc) {
  state.docs.set(doc.key, doc);
  state.activeKey = doc.key;
  const editor = $('editor');
  editor.value = doc.content;
  editor.scrollTop = 0;
  renderDocHeader();
  renderTree();
  renderBookmarks();
  schedulePreview(0);
  updateCounts();
}

/* ── Rendering: account, repositories, branches ───────────────────────────── */

function renderAccount() {
  const label = $('account-login');
  const action = $('account-action');
  if (state.user) {
    label.textContent = state.user.login;
    label.title = state.user.name ? `${state.user.name} (${state.user.login})` : state.user.login;
    action.textContent = 'Sign out';
  } else {
    label.textContent = 'Not connected';
    action.textContent = 'Link';
  }
}

function renderRepoList() {
  const list = $('repo-list');
  list.textContent = '';

  if (!state.repos.length) {
    const empty = create('li');
    empty.appendChild(create('p', 'side-empty', 'No repositories connected yet.'));
    list.appendChild(empty);
    return;
  }

  for (const repo of state.repos) {
    const item = create('li', 'repo-item');
    if (repo.fullName === state.activeRepo) item.classList.add('active');
    item.appendChild(icon('i-repo'));

    const name = create('span', 'repo-name');
    name.appendChild(create('span', 'repo-owner', `${repo.owner}/`));
    name.appendChild(document.createTextNode(repo.name));
    name.title = repo.fullName;
    item.appendChild(name);

    if (repo.private) item.appendChild(icon('i-lock', 'icon-lock'));

    const remove = create('button', 'repo-remove', '×');
    remove.type = 'button';
    remove.title = `Disconnect ${repo.fullName}`;
    remove.setAttribute('aria-label', `Disconnect ${repo.fullName}`);
    remove.addEventListener('click', (event) => {
      event.stopPropagation();
      disconnectRepo(repo.fullName);
    });
    item.appendChild(remove);

    item.addEventListener('click', () => selectRepo(repo.fullName));
    list.appendChild(item);
  }
}

/* ── Bookmarks ───────────────────────────────────────────────────────────── */

const bookmarkKey = (b) => `${b.repo}@${b.branch}:${b.path}`;

const findBookmark = (repoFullName, branch, path) =>
  state.bookmarks.findIndex((b) => b.repo === repoFullName && b.branch === branch && b.path === path);

/** toggleBookmark pins or unpins a document, returning true when now pinned. */
function toggleBookmark(repoFullName, branch, path) {
  if (!repoFullName) {
    toast('Save this draft to a repository before bookmarking it.', 'error');
    return false;
  }
  const at = findBookmark(repoFullName, branch, path);
  if (at >= 0) {
    state.bookmarks.splice(at, 1);
  } else {
    state.bookmarks.push({ repo: repoFullName, branch, path });
  }
  saveStore();
  renderBookmarks();
  renderDocHeader();
  return at < 0;
}

function renderBookmarks() {
  const list = $('bookmark-list');
  const empty = $('bookmarks-empty');
  list.textContent = '';

  if (!state.bookmarks.length) {
    empty.hidden = false;
    return;
  }
  empty.hidden = true;

  for (const bookmark of state.bookmarks) {
    const item = create('li', 'bookmark-item');
    const connected = repoOf(bookmark.repo);
    if (!connected) item.classList.add('missing');

    const active = activeDoc();
    if (active && active.repoFullName === bookmark.repo &&
        active.branch === bookmark.branch && active.path === bookmark.path) {
      item.classList.add('active');
    }

    item.appendChild(icon('i-bookmark-fill'));

    const meta = create('div', 'bookmark-meta');
    meta.appendChild(create('div', 'bookmark-name', baseName(bookmark.path)));
    // Show enough context to tell two same-named files apart: the folder, plus
    // the repository and branch whenever they are not the ones in view.
    const where = [];
    if (dirName(bookmark.path)) where.push(dirName(bookmark.path));
    if (bookmark.repo !== state.activeRepo) where.push(bookmark.repo);
    const repo = connected;
    if (repo && bookmark.branch !== repo.branch) where.push(bookmark.branch);
    meta.appendChild(create('div', 'bookmark-where',
      connected ? (where.join(' · ') || bookmark.branch) : 'repository not connected'));
    item.appendChild(meta);

    const remove = create('button', 'bookmark-remove', '×');
    remove.type = 'button';
    remove.title = `Remove bookmark for ${bookmark.path}`;
    remove.setAttribute('aria-label', remove.title);
    remove.addEventListener('click', (event) => {
      event.stopPropagation();
      toggleBookmark(bookmark.repo, bookmark.branch, bookmark.path);
    });
    item.appendChild(remove);

    item.title = `${bookmark.repo} · ${bookmark.branch} · ${bookmark.path}`;
    item.addEventListener('click', () => openBookmark(bookmark));
    list.appendChild(item);
  }
}

/** openBookmark jumps to a pinned document, switching repository and branch when
 *  the bookmark points somewhere other than the current view. */
async function openBookmark(bookmark) {
  const repo = repoOf(bookmark.repo);
  if (!repo) {
    toast(`${bookmark.repo} is not connected any more — reconnect it to open this bookmark.`, 'error');
    return;
  }

  const branchChanged = repo.branch !== bookmark.branch;
  if (branchChanged) {
    repo.branch = bookmark.branch;
    saveStore();
  }

  if (state.activeRepo !== bookmark.repo) {
    await selectRepo(bookmark.repo);
  } else if (branchChanged) {
    renderBranches();
    await loadTree(repo);
  }

  // Reveal the document's folder in the tree so its position is obvious.
  let dir = dirName(bookmark.path);
  while (dir) {
    state.expanded.add(dir);
    dir = dirName(dir);
  }
  saveStore();

  await openDocument(bookmark.path);
  renderBookmarks();
}

function renderBranches() {
  const select = $('branch-select');
  select.textContent = '';
  const repo = activeRepo();

  if (!repo) {
    select.disabled = true;
    select.appendChild(create('option', null, 'No repository selected'));
    return;
  }
  const names = state.branches.length ? state.branches.map((b) => b.name) : [repo.branch || repo.defaultBranch];
  select.disabled = false;
  for (const name of names) {
    const option = create('option', null, name);
    option.value = name;
    if (name === repo.branch) option.selected = true;
    select.appendChild(option);
  }
}

/* ── Rendering: document header and counts ───────────────────────────────── */

function renderDocHeader() {
  const doc = activeDoc();
  const title = $('doc-title');
  const status = $('doc-status');
  const location = $('doc-location');

  const bookmarkBtn = $('toggle-bookmark');

  if (!doc) {
    title.textContent = 'No document open';
    status.textContent = '—';
    status.className = 'doc-status';
    location.textContent = '';
    $('delete-document').disabled = true;
    $('commit-push').disabled = true;
    $('rename-document').disabled = true;
    bookmarkBtn.disabled = true;
    bookmarkBtn.classList.remove('active');
    bookmarkBtn.setAttribute('aria-pressed', 'false');
    return;
  }

  const pinned = !!doc.repoFullName && findBookmark(doc.repoFullName, doc.branch, doc.path) >= 0;
  bookmarkBtn.disabled = !doc.repoFullName;
  bookmarkBtn.classList.toggle('active', pinned);
  bookmarkBtn.setAttribute('aria-pressed', String(pinned));
  bookmarkBtn.title = pinned ? 'Remove bookmark' : 'Bookmark this document';
  bookmarkBtn.setAttribute('aria-label', bookmarkBtn.title);
  bookmarkBtn.querySelector('use').setAttribute('href', pinned ? '#i-bookmark-fill' : '#i-bookmark');

  title.textContent = baseName(doc.path);
  title.title = doc.path;
  $('rename-document').disabled = false;
  $('delete-document').disabled = !doc.repoFullName;
  $('commit-push').disabled = false;

  if (doc.isNew) {
    status.textContent = doc.repoFullName ? 'New file' : 'Local draft';
    status.className = 'doc-status is-dirty';
  } else if (isDirty(doc)) {
    status.textContent = 'Unsaved changes';
    status.className = 'doc-status is-dirty';
  } else {
    status.textContent = 'Saved';
    status.className = 'doc-status is-saved';
  }

  location.textContent = doc.repoFullName
    ? `${doc.repoFullName} · ${doc.branch} · ${dirName(doc.path) || '/'}`
    : 'Not connected to a repository yet';
}

function updateCounts() {
  const text = $('editor').value;
  const words = text.trim() ? text.trim().split(/\s+/).length : 0;
  $('counts').textContent = `${words.toLocaleString()} ${words === 1 ? 'word' : 'words'} · ${text.length.toLocaleString()} characters`;
}

/* ── Rendering: document tree ────────────────────────────────────────────── */

function buildHierarchy(nodes) {
  const root = { path: '', name: '', type: 'dir', children: [] };
  const dirs = new Map([['', root]]);

  const ensureDir = (path) => {
    if (dirs.has(path)) return dirs.get(path);
    const node = { path, name: baseName(path), type: 'dir', children: [] };
    dirs.set(path, node);
    ensureDir(dirName(path)).children.push(node);
    return node;
  };

  for (const node of nodes) if (node.type === 'dir') ensureDir(node.path);
  for (const node of nodes) {
    if (node.type !== 'file') continue;
    ensureDir(dirName(node.path)).children.push({ ...node, children: null });
  }

  const sortChildren = (node) => {
    if (!node.children) return;
    node.children.sort((a, b) => {
      if ((a.type === 'dir') !== (b.type === 'dir')) return a.type === 'dir' ? -1 : 1;
      return a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: 'base' });
    });
    node.children.forEach(sortChildren);
  };
  sortChildren(root);
  return root;
}

/** matchesFilter keeps a subtree when the folder or any descendant matches. */
function matchesFilter(node, needle) {
  if (!needle) return true;
  if (node.path.toLowerCase().includes(needle)) return true;
  return !!node.children && node.children.some((child) => matchesFilter(child, needle));
}

function renderTree() {
  const list = $('doc-tree');
  const empty = $('tree-empty');
  list.textContent = '';

  const repo = activeRepo();
  if (!repo) {
    empty.hidden = false;
    empty.textContent = state.repos.length
      ? 'Select a repository above to browse its documents.'
      : 'Connect a repository to see its Markdown files.';
    return;
  }

  const needle = state.filter.trim().toLowerCase();

  // Documents created locally but not yet pushed belong in the tree too, so they
  // are merged in before the hierarchy is built and sorted.
  const entries = [...state.tree];
  for (const doc of state.docs.values()) {
    if (doc.repoFullName !== repo.fullName || doc.branch !== repo.branch || !doc.isNew) continue;
    if (state.tree.some((n) => n.path === doc.path)) continue;
    entries.push({ path: doc.path, name: baseName(doc.path), type: 'file', pending: true });
  }
  const root = buildHierarchy(entries);

  if (!root.children.length) {
    empty.hidden = false;
    empty.textContent = state.treeTruncated
      ? 'This repository is too large to list completely.'
      : 'No Markdown files on this branch yet. Create one with “New Document”.';
    return;
  }

  const visible = root.children.filter((child) => matchesFilter(child, needle));
  if (!visible.length) {
    empty.hidden = false;
    empty.textContent = `Nothing matches “${state.filter}”.`;
    return;
  }
  empty.hidden = true;

  for (const child of visible) list.appendChild(renderNode(child, needle, 0));

  if (state.treeTruncated) {
    const warn = create('li');
    warn.appendChild(create('p', 'side-empty', 'Listing was truncated by GitHub; some files are hidden.'));
    list.appendChild(warn);
  }
}

function renderNode(node, needle, depth) {
  const item = create('li', 'tree-item');
  const row = create('div', 'tree-node');
  row.style.paddingLeft = `${8 + depth * 13}px`;
  row.tabIndex = 0;
  row.dataset.path = node.path;
  row.dataset.type = node.type;

  if (node.type === 'dir') {
    item.classList.add('is-dir-item');
    row.classList.add('is-dir');
    // Folders auto-expand while filtering so matches are visible.
    const expanded = needle ? true : state.expanded.has(node.path);
    if (!expanded) item.classList.add('collapsed');

    row.appendChild(icon('i-chevron', 'twisty'));
    row.appendChild(icon('i-folder'));
    row.appendChild(create('span', 'tree-label', node.name));
    row.title = node.path;

    row.addEventListener('click', (event) => {
      event.stopPropagation();
      toggleDir(node.path, item);
    });
  } else {
    row.appendChild(create('span', 'twisty-spacer'));
    row.appendChild(icon('i-file'));
    row.appendChild(create('span', 'tree-label', node.name));
    row.title = node.path;

    const repo = activeRepo();
    const key = docKey(repo.fullName, repo.branch, node.path);
    const doc = state.docs.get(key);
    if (key === state.activeKey) row.classList.add('active');
    if (isDirty(doc)) row.appendChild(icon('i-dot', 'dirty-dot'));

    row.addEventListener('click', (event) => {
      event.stopPropagation();
      openDocument(node.path, { pending: node.pending });
    });
    row.addEventListener('keydown', (event) => {
      if (event.key === 'Enter' || event.key === ' ') {
        event.preventDefault();
        openDocument(node.path, { pending: node.pending });
      }
    });
  }

  attachDragHandlers(row, node);
  row.addEventListener('contextmenu', (event) => {
    event.preventDefault();
    showContextMenu(event, node);
  });

  item.appendChild(row);

  if (node.children) {
    const children = create('ul', 'tree-children');
    for (const child of node.children) {
      if (!matchesFilter(child, needle)) continue;
      children.appendChild(renderNode(child, needle, depth + 1));
    }
    item.appendChild(children);
  }
  return item;
}

function toggleDir(path, item) {
  if (state.expanded.has(path)) state.expanded.delete(path);
  else state.expanded.add(path);
  item.classList.toggle('collapsed');
  saveStore();
}

/* ── Drag and drop: move documents between folders ───────────────────────── */

let dragging = null;

function attachDragHandlers(row, node) {
  row.draggable = true;

  row.addEventListener('dragstart', (event) => {
    dragging = node;
    row.classList.add('dragging');
    event.dataTransfer.effectAllowed = 'move';
    event.dataTransfer.setData('text/plain', node.path);
  });
  row.addEventListener('dragend', () => {
    dragging = null;
    row.classList.remove('dragging');
    document.querySelectorAll('.drop-target').forEach((n) => n.classList.remove('drop-target'));
  });

  if (node.type !== 'dir') return;

  row.addEventListener('dragover', (event) => {
    if (!canDropInto(node)) return;
    event.preventDefault();
    event.dataTransfer.dropEffect = 'move';
    row.classList.add('drop-target');
  });
  row.addEventListener('dragleave', () => row.classList.remove('drop-target'));
  row.addEventListener('drop', (event) => {
    event.preventDefault();
    row.classList.remove('drop-target');
    if (!canDropInto(node)) return;
    movePath(dragging.path, joinPath(node.path, baseName(dragging.path)));
  });
}

function canDropInto(target) {
  if (!dragging || dragging.path === target.path) return false;
  if (dirName(dragging.path) === target.path) return false;       // already there
  if (`${target.path}/`.startsWith(`${dragging.path}/`)) return false; // into itself
  return true;
}

// Dropping on the empty area of the tree moves an item to the repository root.
function setupRootDropZone() {
  const list = $('doc-tree');
  list.addEventListener('dragover', (event) => {
    if (!dragging || dirName(dragging.path) === '') return;
    event.preventDefault();
    event.dataTransfer.dropEffect = 'move';
  });
  list.addEventListener('drop', (event) => {
    if (event.target.closest('.tree-node')) return; // a folder handled it
    if (!dragging || dirName(dragging.path) === '') return;
    event.preventDefault();
    movePath(dragging.path, baseName(dragging.path));
  });
}

/* ── Context menu ────────────────────────────────────────────────────────── */

let contextMenu = null;

function hideContextMenu() {
  if (contextMenu) {
    contextMenu.remove();
    contextMenu = null;
  }
}

function showContextMenu(event, node) {
  hideContextMenu();
  const repo = activeRepo();
  if (!repo) return;

  const menu = create('div', 'menu');
  menu.setAttribute('role', 'menu');
  const add = (label, handler) => {
    const button = create('button', 'menu-item', label);
    button.type = 'button';
    button.addEventListener('click', () => {
      hideContextMenu();
      handler();
    });
    menu.appendChild(button);
  };

  const folder = node.type === 'dir' ? node.path : dirName(node.path);
  if (node.type === 'file') {
    add('Open', () => openDocument(node.path, { pending: node.pending }));
    const pinned = findBookmark(repo.fullName, repo.branch, node.path) >= 0;
    add(pinned ? 'Remove bookmark' : 'Bookmark', () => {
      toggleBookmark(repo.fullName, repo.branch, node.path);
    });
  }
  add('New document here…', () => newDocument(folder));
  add('New folder here…', () => newFolder(folder));
  menu.appendChild(create('div', 'menu-sep'));
  add('Rename or move…', () => renamePath(node));
  add(node.type === 'dir' ? 'Delete folder…' : 'Delete…', () => deletePath(node));

  menu.style.position = 'fixed';
  menu.style.left = `${Math.min(event.clientX, window.innerWidth - 230)}px`;
  menu.style.top = `${Math.min(event.clientY, window.innerHeight - 200)}px`;
  document.body.appendChild(menu);
  contextMenu = menu;
}

/* ── Repository operations ───────────────────────────────────────────────── */

async function selectRepo(fullName) {
  const repo = repoOf(fullName);
  if (!repo) return;
  state.activeRepo = fullName;
  saveStore();
  renderRepoList();
  renderBranches();
  await Promise.all([loadBranches(repo), loadTree(repo)]);
}

async function loadBranches(repo) {
  try {
    const data = await api(repoPath(repo, '/branches'));
    state.branches = data.branches || [];
    if (!state.branches.some((b) => b.name === repo.branch)) {
      repo.branch = repo.defaultBranch;
      saveStore();
    }
  } catch (err) {
    state.branches = [];
    if (err.status !== 401) failToast(err);
  }
  renderBranches();
}

async function loadTree(repo) {
  if (!repo) return;
  const empty = $('tree-empty');
  empty.hidden = false;
  empty.textContent = 'Loading…';
  try {
    const params = new URLSearchParams({ ref: repo.branch });
    const data = await api(repoPath(repo, `/tree?${params}`));
    state.tree = data.nodes || [];
    state.treeTruncated = !!data.truncated;
    if (data.ref && data.ref !== repo.branch) {
      repo.branch = data.ref;
      saveStore();
    }
  } catch (err) {
    state.tree = [];
    state.treeTruncated = false;
    if (err.status !== 401) failToast(err);
  }
  syncShasFromTree();
  renderTree();
  renderBranches();
  renderDocHeader();
}

function disconnectRepo(fullName) {
  state.repos = state.repos.filter((r) => r.fullName !== fullName);
  if (state.activeRepo === fullName) {
    state.activeRepo = state.repos.length ? state.repos[0].fullName : null;
    state.tree = [];
    state.branches = [];
  }
  saveStore();
  renderRepoList();
  renderBranches();
  const repo = activeRepo();
  if (repo) loadTree(repo);
  else renderTree();
  toast(`Disconnected ${fullName}.`);
}

function connectRepo(repo) {
  if (repoOf(repo.full_name)) return repoOf(repo.full_name);
  const entry = {
    fullName: repo.full_name,
    owner: repo.owner,
    name: repo.name,
    private: !!repo.private,
    canPush: !!repo.can_push,
    defaultBranch: repo.default_branch || 'main',
    branch: repo.default_branch || 'main',
  };
  state.repos.push(entry);
  state.repos.sort((a, b) => a.fullName.localeCompare(b.fullName));
  saveStore();
  renderRepoList();
  return entry;
}

/* ── Repository picker dialog ────────────────────────────────────────────── */

let pickerCache = null;

async function openRepoPicker() {
  $('repos-error').hidden = true;
  $('repo-search').value = '';
  showDialog('dialog-repos');
  renderPicker(null, 'Loading your repositories…');

  if (!pickerCache) {
    try {
      // Two pages cover 200 repositories, which is plenty for the picker; the
      // search box filters what is shown.
      const first = await api('/api/repos?page=1');
      let all = first.repos || [];
      if (first.has_more) {
        const second = await api('/api/repos?page=2');
        all = all.concat(second.repos || []);
      }
      pickerCache = all;
    } catch (err) {
      renderPicker([], null);
      $('repos-error').textContent = err.message;
      $('repos-error').hidden = false;
      return;
    }
  }
  renderPicker(pickerCache, null);
}

function renderPicker(repos, loadingMessage) {
  const list = $('repo-picker');
  list.textContent = '';

  if (loadingMessage) {
    list.appendChild(create('li', 'picker-loading', loadingMessage));
    return;
  }
  if (!repos) return;

  const needle = $('repo-search').value.trim().toLowerCase();
  const shown = repos.filter((r) => !needle || r.full_name.toLowerCase().includes(needle));

  if (!shown.length) {
    list.appendChild(create('li', 'picker-none',
      needle ? 'No repositories match that search.' : 'No repositories are visible to this token.'));
    return;
  }

  for (const repo of shown.slice(0, 200)) {
    const item = create('li', 'picker-item');
    const meta = create('div', 'picker-meta');
    meta.appendChild(create('div', 'picker-name', repo.full_name));
    const sub = [repo.default_branch];
    if (repo.description) sub.push(repo.description);
    meta.appendChild(create('div', 'picker-sub', sub.join(' · ')));
    item.appendChild(meta);

    if (repo.private) item.appendChild(create('span', 'pill pill-private', 'Private'));
    if (!repo.can_push) item.appendChild(create('span', 'pill pill-readonly', 'Read only'));

    const connected = !!repoOf(repo.full_name);
    const button = create('button', `picker-btn${connected ? ' connected' : ''}`, connected ? 'Connected' : 'Connect');
    button.type = 'button';
    button.disabled = connected;
    button.addEventListener('click', () => {
      const entry = connectRepo(repo);
      button.textContent = 'Connected';
      button.classList.add('connected');
      button.disabled = true;
      if (!state.activeRepo) selectRepo(entry.fullName);
      toast(`Connected ${repo.full_name}.`, 'ok');
    });
    item.appendChild(button);
    list.appendChild(item);
  }
}

/* ── Opening and editing documents ──────────────────────────────────────── */

async function openDocument(path, { pending = false } = {}) {
  const repo = activeRepo();
  if (!repo) return;
  const key = docKey(repo.fullName, repo.branch, path);

  const existing = state.docs.get(key);
  if (existing) {
    setActiveDoc(existing);
    return;
  }
  if (pending) return; // an unpushed file with no in-memory document is a bug, not a fetch

  try {
    const params = new URLSearchParams({ path, ref: repo.branch });
    const file = await api(repoPath(repo, `/file?${params}`));
    const doc = makeDoc({
      repoFullName: repo.fullName,
      branch: repo.branch,
      path: file.path,
      content: file.content,
      sha: file.sha,
    });

    // A draft from a previous visit wins, but only if it was based on the same
    // commit — otherwise the file moved on and the draft would be misleading.
    const draft = readDraft(doc.key);
    if (draft && draft.sha === doc.sha && draft.content !== doc.content) {
      doc.content = draft.content;
      const restored = toast('Restored an unsaved draft from this browser.', 'info', { timeout: 12000 });
      const discard = create('button', 'toast-close', 'Discard draft');
      discard.type = 'button';
      discard.style.textDecoration = 'underline';
      discard.addEventListener('click', () => {
        doc.content = doc.baseContent;
        dropDraft(doc);
        if (state.activeKey === doc.key) setActiveDoc(doc);
        restored.remove();
      });
      restored.insertBefore(discard, restored.lastChild);
    }
    setActiveDoc(doc);
  } catch (err) {
    if (err.status !== 401) failToast(err);
  }
}

function onEditorInput() {
  const doc = activeDoc();
  if (doc) {
    doc.content = $('editor').value;
    queueDraftSave(doc);
    renderDocHeader();
    // Rebuilding the sidebar on every keystroke is pure waste: the only thing
    // that can change there is the unsaved-changes dot. On a large document that
    // per-keystroke DOM rebuild is felt directly.
    const dirty = isDirty(doc);
    if (doc.shownDirty !== dirty) {
      doc.shownDirty = dirty;
      renderTree();
    }
  }
  updateCounts();
  schedulePreview();
}

/* ── Preview ─────────────────────────────────────────────────────────────── */

let previewTimer = null;
let previewAbort = null;
let lastRendered = null;

/* Rendering happens on the server, so switching documents used to mean a network
 * round trip every time — including back to one just viewed. Keep the last few
 * results keyed by document so revisits paint immediately. */
const renderCache = new Map();
const RENDER_CACHE_LIMIT = 12;

function cacheRender(key, markdown, html) {
  if (!key) return;
  renderCache.delete(key);
  renderCache.set(key, { markdown, html });
  // Map preserves insertion order, so the first key is the least recently used.
  while (renderCache.size > RENDER_CACHE_LIMIT) {
    renderCache.delete(renderCache.keys().next().value);
  }
}

function schedulePreview(delay = 320) {
  clearTimeout(previewTimer);
  previewTimer = setTimeout(renderPreview, delay);
}

async function renderPreview() {
  const markdown = $('editor').value;
  const preview = $('preview');

  if (!markdown.trim()) {
    preview.textContent = '';
    preview.appendChild(create('p', 'preview-empty', 'Nothing to preview yet.'));
    lastRendered = '';
    return;
  }
  if (markdown === lastRendered) return;

  const doc = activeDoc();
  const cacheKey = doc ? doc.key : '';

  // A revisited document with unchanged text needs no server round trip.
  const cached = renderCache.get(cacheKey);
  if (cached && cached.markdown === markdown) {
    preview.innerHTML = cached.html;
    lastRendered = markdown;
    requestAnimationFrame(alignPreviewToEditor);
    return;
  }

  if (previewAbort) previewAbort.abort();
  previewAbort = new AbortController();
  const signal = previewAbort.signal;

  const body = { markdown };
  if (doc && doc.repoFullName) {
    const repo = repoOf(doc.repoFullName);
    if (repo) {
      body.context = { owner: repo.owner, repo: repo.name, ref: doc.branch, path: doc.path };
    }
  }

  try {
    const response = await fetch('/api/render', {
      method: 'POST',
      credentials: 'same-origin',
      headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': state.csrf || '' },
      body: JSON.stringify(body),
      signal,
    });
    if (response.status === 401) { redirectToLogin(); return; }
    if (!response.ok) throw new Error(`preview failed (${response.status})`);
    const data = await response.json();
    if (signal.aborted) return;
    // The HTML was sanitized server-side against a strict allow-list.
    preview.innerHTML = data.html;
    lastRendered = markdown;
    cacheRender(cacheKey, markdown, data.html);
    // The preview's height just changed; re-map the editor's position onto it.
    requestAnimationFrame(alignPreviewToEditor);
  } catch (err) {
    if (err.name !== 'AbortError') console.warn('preview:', err);
  }
}

/* ── Creating, renaming, deleting ────────────────────────────────────────── */

async function newDocument(folder = '') {
  const repo = activeRepo();
  const suggested = joinPath(folder, 'untitled.md');
  const path = await promptDialog({
    title: 'New document',
    hint: repo
      ? `Created on ${repo.fullName} (${repo.branch}) when you push. Use “/” for folders.`
      : 'Not connected to a repository — this stays a local draft until you connect one.',
    value: suggested,
    confirmLabel: 'Create',
    validate: (value) => validatePath(value),
  });
  if (!path) return;

  const normalized = normalizePath(path);
  if (repo) {
    const key = docKey(repo.fullName, repo.branch, normalized);
    if (state.tree.some((n) => n.path === normalized) || state.docs.has(key)) {
      toast(`${normalized} already exists — opening it instead.`);
      openDocument(normalized);
      return;
    }
  }

  const template = `# ${baseName(normalized).replace(MARKDOWN_EXT, '')}\n\n`;
  const doc = makeDoc({
    repoFullName: repo ? repo.fullName : null,
    branch: repo ? repo.branch : '',
    path: normalized,
    content: template,
    isNew: true,
  });
  if (dirName(normalized)) state.expanded.add(dirName(normalized));
  setActiveDoc(doc);
  saveDraft(doc);
  $('editor').focus();
  $('editor').setSelectionRange(template.length, template.length);
}

async function newFolder(parent = '') {
  const repo = activeRepo();
  if (!repo) {
    toast('Connect a repository before creating folders.', 'error');
    return;
  }
  const name = await promptDialog({
    title: 'New folder',
    hint: 'Git has no empty folders, so a small .gitkeep file is committed to hold the folder.',
    value: joinPath(parent, 'new-folder'),
    confirmLabel: 'Create folder',
    validate: (value) => validatePath(value, { requireMarkdown: false }),
  });
  if (!name) return;

  const folder = normalizePath(name);
  const button = $('new-folder');
  busy(button, true);
  try {
    await api(repoPath(repo, '/file'), {
      method: 'PUT',
      body: {
        path: joinPath(folder, '.gitkeep'),
        content: '',
        message: `Create folder ${folder}`,
        branch: repo.branch,
        sha: '',
      },
    });
    state.expanded.add(folder);
    saveStore();
    await loadTree(repo);
    toast(`Created folder ${folder}.`, 'ok');
  } catch (err) {
    if (err.status !== 401) failToast(err);
  } finally {
    busy(button, false);
  }
}

async function renamePath(node) {
  const repo = activeRepo();
  if (!repo) return;
  const isDir = node.type === 'dir';

  // An unpushed document only exists locally, so renaming is a local operation.
  const key = docKey(repo.fullName, repo.branch, node.path);
  const localDoc = state.docs.get(key);
  if (!isDir && localDoc && localDoc.isNew) {
    const target = await promptDialog({
      title: 'Rename draft',
      hint: 'This document has not been pushed yet, so the change is local only.',
      value: node.path,
      confirmLabel: 'Rename',
      validate: (value) => validatePath(value),
    });
    if (!target) return;
    state.docs.delete(key);
    dropDraft(localDoc);
    const renamed = makeDoc({
      repoFullName: repo.fullName,
      branch: repo.branch,
      path: normalizePath(target),
      content: localDoc.content,
      isNew: true,
    });
    setActiveDoc(renamed);
    saveDraft(renamed);
    return;
  }

  const target = await promptDialog({
    title: isDir ? 'Rename or move folder' : 'Rename or move document',
    hint: `Committed to ${repo.fullName} (${repo.branch}) as a single move.`,
    value: node.path,
    confirmLabel: 'Move',
    validate: (value) => validatePath(value, { requireMarkdown: !isDir }),
  });
  if (!target || normalizePath(target) === node.path) return;
  await movePath(node.path, normalizePath(target));
}

async function movePath(from, to) {
  const repo = activeRepo();
  if (!repo) return;
  try {
    await api(repoPath(repo, '/move'), {
      method: 'POST',
      body: { from, to, branch: repo.branch, message: `Move ${from} to ${to}` },
    });

    // Re-key any open documents that moved.
    for (const [key, doc] of [...state.docs]) {
      if (doc.repoFullName !== repo.fullName || doc.branch !== repo.branch) continue;
      if (doc.path !== from && !doc.path.startsWith(`${from}/`)) continue;
      state.docs.delete(key);
      dropDraft(doc);
      doc.path = doc.path === from ? to : to + doc.path.slice(from.length);
      doc.key = docKey(repo.fullName, repo.branch, doc.path);
      // A move reuses the same blob, so the SHA the next push needs is unchanged.
      state.docs.set(doc.key, doc);
      saveDraft(doc);
      if (state.activeKey === key) state.activeKey = doc.key;
    }
    if (dirName(to)) state.expanded.add(dirName(to));
    saveStore();

    await loadTree(repo);
    renderDocHeader();
    toast(`Moved ${from} → ${to}.`, 'ok');
  } catch (err) {
    if (err.status !== 401) failToast(err);
  }
}

/** syncShasFromTree fills in blob SHAs that a batch commit left unknown.
 *
 *  It deliberately touches only documents whose SHA is missing. Refreshing the
 *  SHA of a document with unsaved edits would silently arm it to overwrite
 *  whatever someone else pushed in the meantime — the stale SHA is what makes
 *  GitHub reject that push, so it must be left alone. */
function syncShasFromTree() {
  const repo = activeRepo();
  if (!repo) return;
  const shaByPath = new Map(state.tree.filter((n) => n.type === 'file').map((n) => [n.path, n.sha]));

  for (const doc of state.docs.values()) {
    if (doc.repoFullName !== repo.fullName || doc.branch !== repo.branch) continue;
    if (doc.isNew || doc.sha) continue;
    const sha = shaByPath.get(doc.path);
    if (sha) doc.sha = sha;
  }
}

async function deletePath(node) {
  const repo = activeRepo();
  if (!repo) return;
  const isDir = node.type === 'dir';

  const key = docKey(repo.fullName, repo.branch, node.path);
  const localDoc = state.docs.get(key);
  if (!isDir && localDoc && localDoc.isNew) {
    state.docs.delete(key);
    dropDraft(localDoc);
    if (state.activeKey === key) {
      state.activeKey = null;
      $('editor').value = '';
      renderDocHeader();
      schedulePreview(0);
      updateCounts();
    }
    renderTree();
    toast(`Discarded draft ${node.path}.`);
    return;
  }

  const confirmed = await confirmDialog({
    title: isDir ? 'Delete this folder?' : 'Delete this document?',
    body: isDir
      ? `Every file under ${node.path} will be removed from ${repo.fullName} (${repo.branch}) in one commit. This cannot be undone from here.`
      : `${node.path} will be removed from ${repo.fullName} (${repo.branch}). This cannot be undone from here.`,
    confirmLabel: isDir ? 'Delete folder' : 'Delete',
  });
  if (!confirmed) return;

  try {
    await api(repoPath(repo, '/delete'), {
      method: 'POST',
      body: {
        path: node.path,
        branch: repo.branch,
        recursive: isDir,
        message: isDir ? `Delete folder ${node.path}` : `Delete ${node.path}`,
        sha: '',
      },
    });

    for (const [docKeyName, doc] of [...state.docs]) {
      if (doc.repoFullName !== repo.fullName || doc.branch !== repo.branch) continue;
      if (doc.path !== node.path && !doc.path.startsWith(`${node.path}/`)) continue;
      dropDraft(doc);
      state.docs.delete(docKeyName);
      if (state.activeKey === docKeyName) {
        state.activeKey = null;
        $('editor').value = '';
      }
    }
    await loadTree(repo);
    renderDocHeader();
    schedulePreview(0);
    updateCounts();
    toast(`Deleted ${node.path}.`, 'ok');
  } catch (err) {
    if (err.status !== 401) failToast(err);
  }
}

/* ── Commit and push ─────────────────────────────────────────────────────── */

function otherDirtyDocs(doc) {
  return [...state.docs.values()].filter((other) =>
    other.key !== doc.key &&
    other.repoFullName === doc.repoFullName &&
    other.branch === doc.branch &&
    isDirty(other));
}

async function commitAndPush() {
  const doc = activeDoc();
  if (!doc) return;

  if (!doc.repoFullName) {
    toast('This draft is not attached to a repository. Connect one, then use “New Document” to place it.', 'error');
    return;
  }
  const repo = repoOf(doc.repoFullName);
  if (!repo) {
    toast('That repository is no longer connected.', 'error');
    return;
  }
  if (!repo.canPush) {
    toast(`You do not have push access to ${repo.fullName}.`, 'error');
    return;
  }
  if (!isDirty(doc)) {
    toast('No changes to push.');
    return;
  }

  const others = otherDirtyDocs(doc);
  $('commit-target').textContent = others.length
    ? `${doc.path} and ${others.length} other unsaved ${others.length === 1 ? 'document' : 'documents'} → ${repo.fullName} (${repo.branch})`
    : `${doc.path} → ${repo.fullName} (${repo.branch})`;

  const input = $('commit-message');
  input.value = doc.isNew ? `Create ${doc.path}` : `Update ${doc.path}`;
  $('commit-error').hidden = true;

  const form = $('commit-form');
  const cancels = [...$('dialog-commit').querySelectorAll('[data-close-dialog]')];

  const cleanup = () => {
    form.removeEventListener('submit', onSubmit);
    cancels.forEach((b) => b.removeEventListener('click', onCancel));
    document.removeEventListener('mde:dialog-dismissed', onCancel);
  };
  const onCancel = () => { cleanup(); closeDialog(); };

  async function onSubmit(event) {
    event.preventDefault();
    const message = input.value.trim() || `Update ${doc.path}`;
    const submit = $('commit-confirm');
    busy(submit, true);
    try {
      if (others.length) await pushBatch(repo, [doc, ...others], message);
      else await pushSingle(repo, doc, message);
      cleanup();
      closeDialog();
    } catch (err) {
      $('commit-error').textContent = err.message;
      $('commit-error').hidden = false;
    } finally {
      busy(submit, false);
    }
  }

  form.addEventListener('submit', onSubmit);
  cancels.forEach((b) => b.addEventListener('click', onCancel));
  document.addEventListener('mde:dialog-dismissed', onCancel);
  showDialog('dialog-commit');
  input.select();
}

async function pushSingle(repo, doc, message) {
  const result = await api(repoPath(repo, '/file'), {
    method: 'PUT',
    body: {
      path: doc.path,
      content: doc.content,
      message,
      branch: repo.branch,
      sha: doc.sha || '',
    },
  });
  markPushed(doc, result.file_sha || null);
  await loadTree(repo);
  toast(`Pushed ${doc.path} to ${repo.fullName}.`, 'ok',
    result.html_url ? { link: { href: result.html_url, label: 'View commit' } } : {});
}

async function pushBatch(repo, docs, message) {
  const result = await api(repoPath(repo, '/commit'), {
    method: 'POST',
    body: {
      branch: repo.branch,
      message,
      changes: docs.map((doc) => ({ path: doc.path, content: doc.content })),
    },
  });
  docs.forEach((doc) => markPushed(doc, null));
  // A tree commit reports no per-file SHAs; loadTree() refills them.
  await loadTree(repo);
  toast(`Pushed ${docs.length} documents to ${repo.fullName}.`, 'ok',
    result.html_url ? { link: { href: result.html_url, label: 'View commit' } } : {});
}

function markPushed(doc, newSha) {
  doc.baseContent = doc.content;
  doc.isNew = false;
  // A null SHA means the caller could not learn it (batch commits); leaving it
  // empty lets syncShasFromTree() fill it from the refreshed tree.
  doc.sha = newSha || null;
  dropDraft(doc);
  state.docs.set(doc.key, doc);
  renderDocHeader();
  renderTree();
}

/* ── Import and export ───────────────────────────────────────────────────── */

function importFromFile(file) {
  if (!file) return;
  if (file.size > 2 * 1024 * 1024) {
    toast('That file is larger than 2 MiB.', 'error');
    return;
  }
  const reader = new FileReader();
  reader.onerror = () => toast('Could not read that file.', 'error');
  reader.onload = () => {
    const text = String(reader.result || '');
    const repo = activeRepo();
    const name = MARKDOWN_EXT.test(file.name) ? file.name : `${file.name}.md`;
    const doc = makeDoc({
      repoFullName: repo ? repo.fullName : null,
      branch: repo ? repo.branch : '',
      path: normalizePath(name),
      content: text,
      isNew: true,
    });
    setActiveDoc(doc);
    saveDraft(doc);
    toast(repo
      ? `Imported ${name}. Push it to add it to ${repo.fullName}.`
      : `Imported ${name} as a local draft.`, 'ok');
  };
  reader.readAsText(file);
}

function buildExportMenu() {
  const menu = $('export-menu');
  menu.textContent = '';

  for (const format of state.formats) {
    const button = create('button', 'menu-item');
    button.type = 'button';
    button.setAttribute('role', 'menuitem');
    button.appendChild(icon('i-file'));
    const label = create('span');
    label.appendChild(document.createTextNode(format.label));
    label.appendChild(create('small', null, format.description));
    button.appendChild(label);
    button.addEventListener('click', () => {
      hideExportMenu();
      runExport(format);
    });
    menu.appendChild(button);
  }

  menu.appendChild(create('div', 'menu-sep'));

  const pdf = create('button', 'menu-item');
  pdf.type = 'button';
  pdf.setAttribute('role', 'menuitem');
  pdf.appendChild(icon('i-file'));
  const pdfLabel = create('span');
  pdfLabel.appendChild(document.createTextNode('PDF'));
  pdfLabel.appendChild(create('small', null, 'Opens your print dialog — choose “Save as PDF”'));
  pdf.appendChild(pdfLabel);
  pdf.addEventListener('click', () => {
    hideExportMenu();
    exportPDF();
  });
  menu.appendChild(pdf);
}

async function runExport(format) {
  const markdown = $('editor').value;
  if (!markdown.trim()) {
    toast('There is nothing to export yet.', 'error');
    return;
  }
  const doc = activeDoc();
  try {
    const response = await api('/api/export', {
      method: 'POST',
      raw: true,
      body: {
        markdown,
        format: format.format,
        title: doc ? baseName(doc.path) : 'document',
      },
    });
    const blob = await response.blob();
    const filename = filenameFrom(response.headers.get('Content-Disposition'))
      || `${(doc ? baseName(doc.path) : 'document').replace(MARKDOWN_EXT, '')}${format.extension}`;

    const url = URL.createObjectURL(blob);
    const anchor = create('a');
    anchor.href = url;
    anchor.download = filename;
    document.body.appendChild(anchor);
    anchor.click();
    anchor.remove();
    // Give the browser a moment to start the download before revoking.
    setTimeout(() => URL.revokeObjectURL(url), 4000);
    toast(`Exported ${filename}.`, 'ok', { timeout: 3500 });
  } catch (err) {
    if (err.status !== 401) failToast(err);
  }
}

function filenameFrom(header) {
  if (!header) return null;
  const utf8 = /filename\*=UTF-8''([^;]+)/i.exec(header);
  if (utf8) {
    try { return decodeURIComponent(utf8[1]); } catch { /* fall through */ }
  }
  const plain = /filename="?([^";]+)"?/i.exec(header);
  return plain ? plain[1] : null;
}

/** exportPDF prints the preview pane, which the browser can save as a PDF. */
async function exportPDF() {
  const app = $('app');
  const previousView = state.settings.view;
  if (previousView !== 'preview') applyView('preview');
  await renderPreview();
  // Let layout settle before handing control to the print dialog.
  await new Promise((resolve) => setTimeout(resolve, 120));
  window.print();
  if (previousView !== 'preview') applyView(previousView);
  app.focus?.();
}

/* ── Layout, settings, shortcuts ─────────────────────────────────────────── */

function applyView(view) {
  const app = $('app');
  app.classList.remove('view-split', 'view-editor', 'view-preview');
  app.classList.add(`view-${view}`);
  $('setting-view').value = view;
  $('toggle-preview').classList.toggle('active', view !== 'editor');
  if (view !== 'editor') schedulePreview(0);
}

function applySettings() {
  const s = state.settings;
  document.documentElement.style.setProperty('--editor-size', `${s.fontSize}px`);
  document.documentElement.style.setProperty('--editor-fraction', `${s.editorFraction}fr`);
  $('app').classList.toggle('sidebar-hidden', s.sidebarHidden);
  applyView(s.view);
  $('setting-font').value = String(s.fontSize);
  $('setting-sync-scroll').checked = s.syncScroll;
  $('setting-autodraft').checked = s.drafts;
}

function setupDivider() {
  const divider = $('pane-divider');
  const panes = $('panes');
  let dragging = false;

  const applyFraction = (clientX) => {
    const rect = panes.getBoundingClientRect();
    const ratio = Math.min(0.85, Math.max(0.15, (clientX - rect.left) / rect.width));
    state.settings.editorFraction = Number((ratio / (1 - ratio)).toFixed(3));
    document.documentElement.style.setProperty('--editor-fraction', `${state.settings.editorFraction}fr`);
  };

  divider.addEventListener('pointerdown', (event) => {
    dragging = true;
    divider.setPointerCapture(event.pointerId);
    event.preventDefault();
  });
  divider.addEventListener('pointermove', (event) => {
    if (dragging) applyFraction(event.clientX);
  });
  divider.addEventListener('pointerup', (event) => {
    dragging = false;
    divider.releasePointerCapture(event.pointerId);
    saveStore();
  });
  divider.addEventListener('keydown', (event) => {
    const step = 0.12;
    if (event.key === 'ArrowLeft') state.settings.editorFraction = Math.max(0.2, state.settings.editorFraction - step);
    else if (event.key === 'ArrowRight') state.settings.editorFraction = Math.min(5, state.settings.editorFraction + step);
    else return;
    event.preventDefault();
    document.documentElement.style.setProperty('--editor-fraction', `${state.settings.editorFraction}fr`);
    saveStore();
  });
}

/** scrollRange is how far an element can actually scroll. */
const scrollRange = (element) => Math.max(0, element.scrollHeight - element.clientHeight);

/** syncLock names the pane currently driving a scroll, so the follower's own
 *  scroll event cannot bounce back and fight it. */
let syncLock = null;
let syncRelease = 0;

/** followScroll maps one pane's scroll position proportionally onto the other. */
function followScroll(from, to) {
  if (!state.settings.syncScroll) return;
  if (syncLock && syncLock !== from) return;

  const fromRange = scrollRange(from);
  const toRange = scrollRange(to);
  if (fromRange <= 0 || toRange <= 0) return;

  syncLock = from;
  to.scrollTop = (from.scrollTop / fromRange) * toRange;

  clearTimeout(syncRelease);
  syncRelease = setTimeout(() => { syncLock = null; }, 90);
}

/** alignPreviewToEditor re-applies the mapping after the preview is replaced,
 *  which keeps the two panes together while typing near the bottom. */
function alignPreviewToEditor() {
  if (!state.settings.syncScroll || syncLock) return;
  const editor = $('editor');
  const previewPane = $('pane-preview');
  const editorRange = scrollRange(editor);
  const previewRange = scrollRange(previewPane);
  if (editorRange <= 0 || previewRange <= 0) return;
  previewPane.scrollTop = (editor.scrollTop / editorRange) * previewRange;
}

function setupScrollSync() {
  const editor = $('editor');
  const previewPane = $('pane-preview');

  // Scrolling either pane moves the other.
  editor.addEventListener('scroll', () => followScroll(editor, previewPane), { passive: true });
  previewPane.addEventListener('scroll', () => followScroll(previewPane, editor), { passive: true });
}

/* ── Editor conveniences ─────────────────────────────────────────────────── */

/** replaceRange rewrites part of the textarea, keeping native undo intact where
 *  the browser supports execCommand and falling back to a plain splice. */
function replaceRange(start, end, text) {
  const editor = $('editor');
  editor.focus();
  editor.setSelectionRange(start, end);

  let applied = false;
  try {
    applied = text === ''
      ? document.execCommand('delete')
      : document.execCommand('insertText', false, text);
  } catch {
    applied = false;
  }
  if (!applied) {
    const value = editor.value;
    editor.value = value.slice(0, start) + text + value.slice(end);
    editor.setSelectionRange(start + text.length, start + text.length);
  }
  onEditorInput();
}

/** insertText replaces the current selection. */
function insertText(text) {
  const editor = $('editor');
  replaceRange(editor.selectionStart, editor.selectionEnd, text);
}

function wrapSelection(before, after = before, placeholder = '') {
  const editor = $('editor');
  const { selectionStart: start, selectionEnd: end, value } = editor;
  const selected = value.slice(start, end);

  // Toggle the markers off when they are already there.
  if (selected && value.slice(start - before.length, start) === before &&
      value.slice(end, end + after.length) === after) {
    editor.setSelectionRange(start - before.length, end + after.length);
    insertText(selected);
    editor.setSelectionRange(start - before.length, end - before.length);
    return;
  }

  const body = selected || placeholder;
  insertText(before + body + after);
  if (!selected && placeholder) {
    const caret = editor.selectionStart - after.length - placeholder.length;
    editor.setSelectionRange(caret, caret + placeholder.length);
  }
}

const LIST_ITEM = /^(\s*)([-*+]\s+\[[ xX]\]\s+|[-*+]\s+|(\d+)([.)])\s+)(.*)$/;

/** handleEnter continues Markdown lists on the next line. */
function handleEnter(event) {
  const editor = $('editor');
  const { selectionStart: start, selectionEnd: end, value } = editor;
  if (start !== end) return false;

  const lineStart = value.lastIndexOf('\n', start - 1) + 1;
  const line = value.slice(lineStart, start);
  const match = LIST_ITEM.exec(line);
  if (!match) return false;

  const [, indent, marker, number, delimiter, body] = match;
  if (!body.trim()) {
    // Enter on an empty list item ends the list instead of adding another bullet.
    event.preventDefault();
    replaceRange(lineStart, start, '');
    return true;
  }

  event.preventDefault();
  let nextMarker = marker;
  if (number !== undefined) nextMarker = `${Number(number) + 1}${delimiter} `;
  else if (/\[[xX]\]/.test(marker)) nextMarker = marker.replace(/\[[xX]\]/, '[ ]');
  insertText(`\n${indent}${nextMarker}`);
  return true;
}

/** handleTab indents or outdents the selected lines. */
function handleTab(event) {
  const editor = $('editor');
  const { selectionStart: start, selectionEnd: end, value } = editor;
  const lineStart = value.lastIndexOf('\n', start - 1) + 1;
  const multiline = value.slice(start, end).includes('\n');

  if (!multiline && !event.shiftKey) {
    event.preventDefault();
    insertText('  ');
    return;
  }

  event.preventDefault();
  const lineEnd = value.indexOf('\n', end) === -1 ? value.length : value.indexOf('\n', end);
  const block = value.slice(lineStart, lineEnd);
  const updated = block
    .split('\n')
    .map((line) => (event.shiftKey ? line.replace(/^ {1,2}/, '') : `  ${line}`))
    .join('\n');

  replaceRange(lineStart, lineEnd, updated);
  editor.setSelectionRange(lineStart, lineStart + updated.length);
}

function setupEditorKeys() {
  const editor = $('editor');
  editor.addEventListener('keydown', (event) => {
    const mod = event.metaKey || event.ctrlKey;

    if (event.key === 'Tab') { handleTab(event); return; }
    if (event.key === 'Enter' && !mod && !event.shiftKey) { handleEnter(event); return; }
    if (!mod) return;

    switch (event.key.toLowerCase()) {
      case 'b':
        event.preventDefault();
        wrapSelection('**', '**', 'bold text');
        break;
      case 'i':
        event.preventDefault();
        wrapSelection('*', '*', 'italic text');
        break;
      case 'k': {
        event.preventDefault();
        const { selectionStart, selectionEnd, value } = editor;
        const selected = value.slice(selectionStart, selectionEnd);
        insertText(`[${selected || 'link text'}](https://)`);
        break;
      }
      default:
        break;
    }
  });
}

function setupGlobalKeys() {
  document.addEventListener('keydown', (event) => {
    const mod = event.metaKey || event.ctrlKey;

    if (event.key === 'Escape') {
      hideContextMenu();
      hideExportMenu();
      if (openDialog) document.dispatchEvent(new CustomEvent('mde:dialog-dismissed'));
      return;
    }
    if (!mod) return;

    const key = event.key.toLowerCase();
    if (key === 's') {
      event.preventDefault();
      commitAndPush();
    } else if (key === 'p' && !event.shiftKey) {
      event.preventDefault();
      togglePreview();
    } else if (event.key === '\\') {
      event.preventDefault();
      toggleSidebar();
    }
  });
}

function togglePreview() {
  const next = state.settings.view === 'editor' ? 'split' : 'editor';
  state.settings.view = next;
  applyView(next);
  saveStore();
}

function toggleSidebar() {
  state.settings.sidebarHidden = !state.settings.sidebarHidden;
  $('app').classList.toggle('sidebar-hidden', state.settings.sidebarHidden);
  saveStore();
}

function hideExportMenu() {
  $('export-menu').hidden = true;
  $('export-btn').setAttribute('aria-expanded', 'false');
}

/* ── Drag and drop onto the editor ───────────────────────────────────────── */

function setupEditorDrop() {
  const editor = $('editor');
  const stop = (event) => { event.preventDefault(); event.stopPropagation(); };

  editor.addEventListener('dragover', (event) => {
    if (![...event.dataTransfer.types].includes('Files')) return;
    stop(event);
    editor.classList.add('drop-active');
  });
  editor.addEventListener('dragleave', () => editor.classList.remove('drop-active'));
  editor.addEventListener('drop', (event) => {
    if (!event.dataTransfer.files.length) return;
    stop(event);
    editor.classList.remove('drop-active');
    importFromFile(event.dataTransfer.files[0]);
  });
}

/* ── Wiring ──────────────────────────────────────────────────────────────── */

function wireEvents() {
  // Collapsible sidebar sections.
  document.querySelectorAll('[data-toggle-group]').forEach((head) => {
    head.addEventListener('click', () => head.closest('.side-group').classList.toggle('collapsed'));
  });

  $('account-action').addEventListener('click', () => {
    if (state.user) signOut();
    else location.assign('/api/auth/login');
  });

  $('connect-repo').addEventListener('click', openRepoPicker);
  $('repo-search').addEventListener('input', () => renderPicker(pickerCache, null));

  $('branch-select').addEventListener('change', async (event) => {
    const repo = activeRepo();
    if (!repo) return;
    repo.branch = event.target.value;
    saveStore();
    await loadTree(repo);
    // Documents are keyed by branch, so the editor now shows nothing for it.
    const doc = activeDoc();
    if (doc && doc.repoFullName === repo.fullName && doc.branch !== repo.branch) {
      openDocument(doc.path).catch(() => {});
    }
  });

  $('refresh-tree').addEventListener('click', () => {
    const repo = activeRepo();
    if (repo) loadTree(repo);
  });
  $('new-folder').addEventListener('click', () => newFolder(currentFolder()));
  $('tree-filter').addEventListener('input', (event) => {
    state.filter = event.target.value;
    renderTree();
  });

  $('new-document').addEventListener('click', () => newDocument(currentFolder()));
  $('commit-push').addEventListener('click', commitAndPush);
  $('delete-document').addEventListener('click', () => {
    const doc = activeDoc();
    if (doc) deletePath({ path: doc.path, type: 'file' });
  });
  $('rename-document').addEventListener('click', () => {
    const doc = activeDoc();
    if (doc) renamePath({ path: doc.path, type: 'file' });
  });

  // Pinning is offered in two places: the toolbar next to the document title,
  // and the Bookmarks section header.
  const pinActive = () => {
    const doc = activeDoc();
    if (!doc) {
      toast('Open a document first.', 'error');
      return;
    }
    const pinned = toggleBookmark(doc.repoFullName, doc.branch, doc.path);
    if (doc.repoFullName) {
      toast(pinned ? `Bookmarked ${baseName(doc.path)}.` : `Removed bookmark for ${baseName(doc.path)}.`,
        'ok', { timeout: 2600 });
    }
  };
  $('toggle-bookmark').addEventListener('click', pinActive);
  $('bookmark-current').addEventListener('click', pinActive);

  $('toggle-sidebar').addEventListener('click', toggleSidebar);
  $('toggle-preview').addEventListener('click', togglePreview);
  $('toggle-zen').addEventListener('click', () => {
    state.settings.sidebarHidden = true;
    state.settings.view = state.settings.view === 'editor' ? 'split' : 'editor';
    applySettings();
    saveStore();
  });

  $('open-settings').addEventListener('click', () => showDialog('dialog-settings'));
  $('open-help').addEventListener('click', () => showDialog('dialog-help'));
  $('sign-out').addEventListener('click', signOut);

  $('setting-view').addEventListener('change', (event) => {
    state.settings.view = event.target.value;
    applyView(event.target.value);
    saveStore();
  });
  $('setting-font').addEventListener('change', (event) => {
    state.settings.fontSize = Number(event.target.value);
    document.documentElement.style.setProperty('--editor-size', `${state.settings.fontSize}px`);
    saveStore();
  });
  $('setting-sync-scroll').addEventListener('change', (event) => {
    state.settings.syncScroll = event.target.checked;
    saveStore();
  });
  $('setting-autodraft').addEventListener('change', (event) => {
    state.settings.drafts = event.target.checked;
    if (!event.target.checked) {
      Object.keys(localStorage)
        .filter((key) => key.startsWith(DRAFT_PREFIX))
        .forEach((key) => localStorage.removeItem(key));
      toast('Local drafts cleared.');
    }
    saveStore();
  });

  $('import-file').addEventListener('click', () => $('file-input').click());
  $('file-input').addEventListener('change', (event) => {
    importFromFile(event.target.files[0]);
    event.target.value = '';
  });

  $('export-btn').addEventListener('click', (event) => {
    event.stopPropagation();
    const menu = $('export-menu');
    const open = menu.hidden;
    menu.hidden = !open;
    $('export-btn').setAttribute('aria-expanded', String(open));
  });

  $('editor').addEventListener('input', onEditorInput);

  $('overlay').addEventListener('click', () => {
    if (openDialog) document.dispatchEvent(new CustomEvent('mde:dialog-dismissed'));
  });
  document.querySelectorAll('.dialog').forEach((dialog) => {
    dialog.addEventListener('keydown', (event) => {
      if (event.key === 'Escape') document.dispatchEvent(new CustomEvent('mde:dialog-dismissed'));
    });
  });
  // Dialogs with no promise attached (settings, help) close on their own buttons.
  ['dialog-settings', 'dialog-help'].forEach((id) => {
    $(id).querySelectorAll('[data-close-dialog]').forEach((b) => b.addEventListener('click', closeDialog));
  });
  document.addEventListener('mde:dialog-dismissed', () => {
    if (openDialog && ['dialog-settings', 'dialog-help', 'dialog-repos'].includes(openDialog.id)) closeDialog();
  });
  $('dialog-repos').querySelectorAll('[data-close-dialog]').forEach((b) => b.addEventListener('click', closeDialog));

  document.addEventListener('click', () => {
    hideContextMenu();
    hideExportMenu();
  });
  $('export-menu').addEventListener('click', (event) => event.stopPropagation());

  // Make sure a coalesced draft write is not lost when the page goes away.
  // visibilitychange is the reliable one; beforeunload does not always fire.
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'hidden') flushDraftSave();
  });
  window.addEventListener('pagehide', flushDraftSave);

  window.addEventListener('beforeunload', (event) => {
    flushDraftSave();
    const dirty = [...state.docs.values()].some(isDirty);
    if (!dirty) return;
    if (state.settings.drafts) return; // drafts survive a reload
    event.preventDefault();
    event.returnValue = '';
  });
}

/** currentFolder is the folder that new items should land in. */
function currentFolder() {
  const doc = activeDoc();
  return doc ? dirName(doc.path) : '';
}

async function signOut() {
  try {
    await api('/api/auth/logout', { method: 'POST' });
  } catch { /* the cookie is cleared regardless */ }
  location.replace('/login.html');
}

/* ── Boot ────────────────────────────────────────────────────────────────── */

async function boot() {
  wireEvents();
  setupDivider();
  setupScrollSync();
  setupEditorKeys();
  setupGlobalKeys();
  setupEditorDrop();
  setupRootDropZone();
  applySettings();

  let me;
  try {
    me = await api('/api/me');
  } catch (err) {
    document.body.classList.remove('app-loading');
    failToast(err);
    return;
  }
  if (!me.authenticated) {
    redirectToLogin();
    return;
  }

  state.user = { login: me.login, name: me.name, avatarUrl: me.avatar_url };
  state.csrf = me.csrf_token;
  renderAccount();
  renderRepoList();
  restoreDrafts();
  renderBookmarks();
  renderBranches();
  document.body.classList.remove('app-loading');

  try {
    const data = await api('/api/formats');
    state.formats = data.formats || [];
  } catch { state.formats = []; }
  buildExportMenu();

  const repo = activeRepo() || (state.repos.length ? state.repos[0] : null);
  if (repo) {
    state.activeRepo = repo.fullName;
    await selectRepo(repo.fullName);
  } else {
    renderTree();
    renderDocHeader();
    // First run: nudge the user straight into connecting a repository.
    openRepoPicker();
  }

  renderDocHeader();
  updateCounts();
  schedulePreview(0);
}

boot().catch((err) => {
  document.body.classList.remove('app-loading');
  console.error(err);
  failToast(err);
});
