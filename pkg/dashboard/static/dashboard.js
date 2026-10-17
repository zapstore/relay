// Zapstore relay dashboard: token handling, live refresh, copy, navigation state, errors.
(function () {
  const NWT_KEY = 'zapstore_nwt';
  const LIVE_KEY = 'zapstore_live';

  // Guard: go to /login when there is no valid token.
  (function () {
    const raw = localStorage.getItem(NWT_KEY);
    if (!raw) { location.replace('/login'); return; }
    try {
      const event = JSON.parse(raw);
      const exp = (event.tags || []).find(t => t[0] === 'exp');
      if (!exp || parseInt(exp[1], 10) <= Math.floor(Date.now() / 1000)) {
        localStorage.removeItem(NWT_KEY);
        location.replace('/login');
      }
    } catch (_) {
      location.replace('/login');
    }
  })();

  function nwtHeader() {
    const raw = localStorage.getItem(NWT_KEY);
    if (!raw) return null;
    // base64url without padding, as per the NWT spec
    return 'Nostr ' + btoa(raw).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }

  function live() { return localStorage.getItem(LIVE_KEY) !== 'off'; }

  // canRefresh decides whether a live region may reload now. It waits while
  // the tab is hidden, live is off, or the operator is working inside it.
  // The poller is a child of the region, so the check uses the region.
  window.canRefresh = function (el) {
    const region = el.closest('[data-live]') || el;
    if (!live() || document.visibilityState !== 'visible') return false;
    if (region.contains(document.activeElement) && document.activeElement !== document.body) return false;
    if (region.dataset.paused) return false;
    if (region.querySelector('details[open]')) return false;
    const sel = getSelection();
    if (sel && !sel.isCollapsed && region.contains(sel.anchorNode)) return false;
    return true;
  };

  let toastTimer;
  function toast(msg, action) {
    const el = document.getElementById('toast');
    if (!el) return;
    document.getElementById('toast-msg').textContent = msg;
    const slot = document.getElementById('toast-action');
    slot.replaceChildren();
    if (action) slot.append(action);
    el.classList.add('show');
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => el.classList.remove('show'), action ? 12000 : 6000);
  }

  function signInLink() {
    const a = document.createElement('a');
    a.href = '/login';
    a.textContent = 'Sign in again';
    return a;
  }

  function markNav() {
    const section = (location.pathname.split('/')[1] || 'apps').toLowerCase();
    document.querySelectorAll('.nav a').forEach(a => {
      if (a.dataset.section.toLowerCase() === section) a.setAttribute('aria-current', 'page');
      else a.removeAttribute('aria-current');
    });
    const q = document.getElementById('top-q');
    if (q && document.activeElement !== q) {
      q.value = section === 'apps' ? (new URLSearchParams(location.search).get('q') || '') : '';
    }
  }

  document.addEventListener('htmx:configRequest', e => {
    const h = nwtHeader();
    if (h) e.detail.headers['Authorization'] = h;
  });

  document.addEventListener('htmx:responseError', e => {
    const xhr = e.detail.xhr;
    const text = (xhr.responseText || '').trim() || ('Request failed (' + xhr.status + ')');
    if (xhr.status === 401) {
      localStorage.removeItem(NWT_KEY);
      toast('Your session ended: ' + text.replace(/^unauthorized:\s*/, ''), signInLink());
      return;
    }
    if (xhr.status === 429) { toast('Too many requests. Wait a moment and try again.'); return; }
    toast(text);
  });

  document.addEventListener('htmx:sendError', () => {
    toast('The dashboard is unreachable. Check the connection; live updates resume on their own.');
  });

  document.addEventListener('htmx:afterSwap', e => {
    if (e.detail.target.id === 'content' && e.detail.boosted) {
      window.scrollTo(0, 0);
    }
  });

  document.addEventListener('htmx:afterSettle', markNav);
  window.addEventListener('popstate', markNav);

  document.addEventListener('click', e => {
    const copy = e.target.closest('[data-copy]');
    if (copy) {
      e.preventDefault();
      navigator.clipboard.writeText(copy.dataset.copy).then(() => {
        copy.classList.add('done');
        const label = copy.getAttribute('aria-label');
        copy.setAttribute('aria-label', 'Copied');
        setTimeout(() => { copy.classList.remove('done'); copy.setAttribute('aria-label', label); }, 1400);
      }, () => toast('Copy failed. Select the text and copy it by hand.'));
      return;
    }
    if (e.target.closest('#signout')) {
      localStorage.removeItem(NWT_KEY);
      location.href = '/login';
      return;
    }
    const liveBtn = e.target.closest('#live');
    if (liveBtn) {
      localStorage.setItem(LIVE_KEY, live() ? 'off' : 'on');
      liveBtn.setAttribute('aria-pressed', String(live()));
    }
  });

  document.addEventListener('keydown', e => {
    const typing = e.target.closest('input, textarea, select, [contenteditable]');
    if ((e.key === '/' && !typing) || (e.key === 'k' && (e.metaKey || e.ctrlKey))) {
      const q = document.getElementById('top-q');
      if (q) { e.preventDefault(); q.focus(); q.select(); }
    }
  });

  document.addEventListener('DOMContentLoaded', () => {
    const btn = document.getElementById('live');
    if (btn) btn.setAttribute('aria-pressed', String(live()));
    markNav();
  });
})();
