// Shared two-pane file browser. Plain DOM on purpose: the server inlines this
// module into public listing pages, where there is no bundler and no
// framework, and the control panel imports the same file so the panes, the
// preview and the keyboard handling exist exactly once.

/**
 * @typedef {object} BrowserEntry
 * @property {string} name
 * @property {boolean} [dir]
 * @property {number} size
 * @property {number} [files] directories only: how many files are underneath
 * @property {string} [hash] files only
 *
 * @typedef {object} BrowserOptions
 * @property {string} [path] directory to open, "" for the root
 * @property {(dir: string) => Promise<{path: string, entries: BrowserEntry[]}>} list
 * @property {(path: string) => string} href URL that serves the file at `path`
 * @property {(path: string, isDir: boolean) => Promise<unknown>} [remove] its presence turns on delete controls
 * @property {(dir: string) => void} [onNavigate]
 */

const IMAGE = /\.(png|jpe?g|gif|webp|avif|bmp|ico|svg)$/i;
const VIDEO = /\.(mp4|webm|ogv|mov|m4v)$/i;
const AUDIO = /\.(mp3|wav|ogg|oga|flac|m4a|aac)$/i;
const TEXT =
  /\.(txt|md|markdown|json|jsonc|ya?ml|toml|csv|tsv|log|css|js|mjs|ts|tsx|jsx|go|py|rb|rs|sh|sql|xml|html?)$/i;

// Text previews are read into memory, so they need a ceiling a 200 MB log file
// cannot walk through.
const TEXT_LIMIT = 256 * 1024;

/**
 * @param {number} n
 * @returns {string}
 */
export function humanBytes(n) {
  if (n < 1024) return `${n} B`;
  const units = ['KB', 'MB', 'GB', 'TB'];
  let v = n / 1024;
  let i = 0;
  while (v >= 1024 && i < units.length - 1) {
    v /= 1024;
    i += 1;
  }
  return `${v.toFixed(1)} ${units[i]}`;
}

/**
 * @param {string} tag
 * @param {string | null} [className]
 * @param {string} [text]
 * @returns {HTMLElement}
 */
function el(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

/**
 * @param {string} dir
 * @param {string} name
 * @returns {string}
 */
function join(dir, name) {
  return dir ? `${dir}/${name}` : name;
}

/**
 * @param {unknown} err
 * @returns {string}
 */
function message(err) {
  return err instanceof Error ? err.message : String(err);
}

/**
 * @param {HTMLElement} root
 * @param {BrowserOptions} opts
 */
export function mount(root, opts) {
  const remove = opts.remove;

  let dir = opts.path || '';
  /** @type {BrowserEntry[]} */
  let entries = [];
  let filter = '';
  let selected = 0;
  // Bumped on every navigation so a slow listing that resolves after the user
  // has moved on cannot repaint the wrong directory.
  let generation = 0;

  root.classList.add('hb');
  root.replaceChildren();

  const bar = el('div', 'hb-bar');
  const crumbs = el('nav', 'hb-crumbs');
  const filterInput = document.createElement('input');
  filterInput.className = 'hb-filter';
  filterInput.type = 'search';
  filterInput.placeholder = 'Filter';
  filterInput.setAttribute('aria-label', 'Filter files');
  bar.append(crumbs, filterInput);

  const panes = el('div', 'hb-panes');
  const list = el('div', 'hb-list');
  list.tabIndex = 0;
  list.setAttribute('role', 'listbox');
  list.setAttribute('aria-label', 'Files');
  const preview = el('div', 'hb-preview');
  panes.append(list, preview);

  /** @param {string} text */
  const kbd = (text) => el('kbd', null, text);

  const hint = el('div', 'hb-hint');
  hint.append(
    kbd('↑'),
    kbd('↓'),
    document.createTextNode(' move · '),
    kbd('↵'),
    document.createTextNode(' open · '),
    kbd('←'),
    document.createTextNode(' up'),
    ...(remove
      ? [document.createTextNode(' · '), kbd('Del'), document.createTextNode(' delete')]
      : [])
  );

  root.append(bar, panes, hint);

  const visible = () =>
    filter ? entries.filter((e) => e.name.toLowerCase().includes(filter)) : entries;

  function drawCrumbs() {
    crumbs.replaceChildren();
    const parts = dir ? dir.split('/') : [];
    const rootBtn = el('button', null, '/');
    rootBtn.onclick = () => navigate('');
    crumbs.append(rootBtn);
    parts.forEach((part, i) => {
      const target = parts.slice(0, i + 1).join('/');
      const btn = el('button', null, part);
      btn.onclick = () => navigate(target);
      crumbs.append(btn, el('span', null, '/'));
    });
    // The separator appended after the last crumb is one too many.
    if (parts.length && crumbs.lastChild) crumbs.lastChild.remove();
  }

  function drawList() {
    const rows = visible();
    list.replaceChildren();
    if (!rows.length) {
      list.append(el('p', 'hb-empty', filter ? 'Nothing matches.' : 'This folder is empty.'));
      return;
    }
    if (selected >= rows.length) selected = rows.length - 1;
    if (selected < 0) selected = 0;

    rows.forEach((entry, i) => {
      const row = el('div', 'hb-row');
      row.setAttribute('role', 'option');
      row.setAttribute('aria-selected', String(i === selected));
      row.append(
        el('span', 'hb-icon', entry.dir ? '▸' : '·'),
        el('span', 'hb-name', entry.dir ? `${entry.name}/` : entry.name),
        el(
          'span',
          'hb-meta',
          entry.dir ? `${entry.files} · ${humanBytes(entry.size)}` : humanBytes(entry.size)
        )
      );
      row.onclick = () => {
        selected = i;
        open(entry);
      };
      if (remove) {
        const del = el('button', 'hb-del', '✕');
        del.title = entry.dir
          ? `Delete ${entry.name}/ and everything in it`
          : `Delete ${entry.name}`;
        del.setAttribute('aria-label', del.title);
        del.onclick = (ev) => {
          ev.stopPropagation();
          destroy(entry);
        };
        row.append(del);
      }
      list.append(row);
    });
    const active = list.children[selected];
    if (active) active.scrollIntoView({ block: 'nearest' });
  }

  /** @param {BrowserEntry} entry */
  function open(entry) {
    if (entry.dir) {
      navigate(join(dir, entry.name));
      return;
    }
    drawList();
    showPreview(entry);
  }

  /** @param {BrowserEntry} entry */
  function showPreview(entry) {
    preview.replaceChildren();
    const head = el('div', 'hb-preview-head');
    head.append(
      el('span', 'hb-preview-name', entry.name),
      el('span', 'hb-meta', humanBytes(entry.size))
    );
    const body = el('div', 'hb-preview-body');
    preview.append(head, body);

    const url = opts.href(join(dir, entry.name));
    const link = document.createElement('a');
    link.className = 'hb-link';
    link.textContent = 'Open in a new tab';
    link.href = url;
    link.target = '_blank';
    link.rel = 'noreferrer noopener';
    head.append(link);

    if (IMAGE.test(entry.name)) {
      const img = document.createElement('img');
      img.src = url;
      img.alt = entry.name;
      img.loading = 'lazy';
      body.append(img);
      return;
    }
    if (VIDEO.test(entry.name)) {
      const video = document.createElement('video');
      video.src = url;
      video.controls = true;
      video.preload = 'metadata';
      body.append(video);
      return;
    }
    if (AUDIO.test(entry.name)) {
      const audio = document.createElement('audio');
      audio.src = url;
      audio.controls = true;
      audio.preload = 'metadata';
      body.append(audio);
      return;
    }
    if (TEXT.test(entry.name) && entry.size <= TEXT_LIMIT) {
      const pre = el('pre', null, 'Loading…');
      body.append(pre);
      const mine = generation;
      fetch(url)
        .then((res) => (res.ok ? res.text() : Promise.reject(new Error(`HTTP ${res.status}`))))
        .then((text) => {
          if (mine === generation) pre.textContent = text.slice(0, TEXT_LIMIT);
        })
        .catch((err) => {
          if (mine === generation) pre.textContent = `Could not load: ${message(err)}`;
        });
      return;
    }
    body.append(el('p', 'hb-empty', 'No preview for this file type.'));
  }

  /** @param {BrowserEntry} entry */
  async function destroy(entry) {
    if (!remove) return;
    const target = join(dir, entry.name);
    const count = entry.files ?? 0;
    const what = entry.dir
      ? `Delete ${target}/ and all ${count} file${count === 1 ? '' : 's'} in it?`
      : `Delete ${target}?`;
    if (!confirm(`${what}\n\nThis cannot be undone.`)) return;
    try {
      await remove(target, !!entry.dir);
    } catch (err) {
      preview.replaceChildren(el('p', 'hb-empty', `Delete failed: ${message(err)}`));
      return;
    }
    await refresh();
  }

  /** @param {string} next */
  async function navigate(next) {
    dir = next;
    filter = '';
    filterInput.value = '';
    selected = 0;
    preview.replaceChildren(el('p', 'hb-empty', 'Select a file to preview it.'));
    if (opts.onNavigate) opts.onNavigate(dir);
    await refresh();
  }

  async function refresh() {
    const mine = (generation += 1);
    drawCrumbs();
    list.replaceChildren(el('p', 'hb-empty', 'Loading…'));
    try {
      const data = await opts.list(dir);
      if (mine !== generation) return;
      entries = (data && data.entries) || [];
    } catch (err) {
      if (mine !== generation) return;
      entries = [];
      list.replaceChildren(el('p', 'hb-empty', `Could not list: ${message(err)}`));
      return;
    }
    drawList();
  }

  function up() {
    if (!dir) return;
    navigate(dir.split('/').slice(0, -1).join('/'));
  }

  list.addEventListener('keydown', (ev) => {
    const rows = visible();
    switch (ev.key) {
      case 'ArrowDown':
        selected = Math.min(selected + 1, rows.length - 1);
        break;
      case 'ArrowUp':
        selected = Math.max(selected - 1, 0);
        break;
      case 'Home':
        selected = 0;
        break;
      case 'End':
        selected = rows.length - 1;
        break;
      case 'Enter':
      case 'ArrowRight':
        if (rows[selected]) open(rows[selected]);
        return;
      case 'ArrowLeft':
      case 'Backspace':
        ev.preventDefault();
        up();
        return;
      case 'Delete':
        if (rows[selected]) destroy(rows[selected]);
        return;
      default:
        return;
    }
    ev.preventDefault();
    drawList();
    const entry = rows[selected];
    if (entry && !entry.dir) showPreview(entry);
  });

  filterInput.addEventListener('input', () => {
    filter = filterInput.value.trim().toLowerCase();
    selected = 0;
    drawList();
  });
  filterInput.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape') {
      filterInput.value = '';
      filter = '';
      selected = 0;
      drawList();
      list.focus();
    }
    if (ev.key === 'ArrowDown' || ev.key === 'Enter') {
      ev.preventDefault();
      list.focus();
    }
  });

  preview.replaceChildren(el('p', 'hb-empty', 'Select a file to preview it.'));
  refresh();

  return {
    refresh,
    navigate,
    get path() {
      return dir;
    },
  };
}
