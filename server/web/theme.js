'use strict';

(() => {
  const key = 'monitor-theme';
  const allowed = new Set(['light','dark','system']);
  const media = window.matchMedia('(prefers-color-scheme: dark)');
  let preference = 'system';
  try {
    const saved = localStorage.getItem(key);
    if (allowed.has(saved)) preference = saved;
  } catch { /* Theme switching also works when browser storage is unavailable. */ }

  function apply() {
    document.documentElement.dataset.theme = preference === 'system' ? (media.matches ? 'dark' : 'light') : preference;
    for (const select of document.querySelectorAll('[data-theme-select]')) select.value = preference;
  }
  // Run in the document head, before CSS and page content, to prevent a light flash.
  apply();
  document.addEventListener('DOMContentLoaded', () => {
    apply();
    for (const select of document.querySelectorAll('[data-theme-select]')) {
      select.addEventListener('change', () => {
        preference = allowed.has(select.value) ? select.value : 'system';
        try { localStorage.setItem(key,preference); } catch { /* Keep the choice for this page. */ }
        apply();
      });
    }
  });
  media.addEventListener('change', () => { if (preference === 'system') apply(); });
  window.addEventListener('storage', event => {
    if (event.key === key || event.key === null) {
      preference = allowed.has(event.newValue) ? event.newValue : 'system';
      apply();
    }
  });
})();
