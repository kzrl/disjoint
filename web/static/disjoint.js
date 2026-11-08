// disjoint.js — Disjoint's shared vanilla JS.
//
// Pasting images (Ctrl/Cmd+V) and drag-and-dropping files into a post
// body textarea attaches them to the form's existing `files` input, so
// everything flows through the standard multipart upload pipeline
// (server-side content sniffing, allowlist, transactional save). There
// is no separate upload endpoint; the preview strip below the file
// input is just a live mirror of input.files.
//
// Any <form> containing both a textarea[name="body"] and a
// input[type="file"][name="files"] is bound automatically.

(function () {
  'use strict';

  var MAX_BYTES = 50 * 1024 * 1024; // mirrors maxUploadSize in internal/handlers/uploads.go

  function formatBytes(n) {
    if (n < 1024) return n + ' B';
    if (n < 1024 * 1024) return (n / 1024).toFixed(1) + ' KB';
    return (n / (1024 * 1024)).toFixed(1) + ' MB';
  }

  function bindForm(form) {
    var textarea = form.querySelector('textarea[name="body"]');
    var fileInput = form.querySelector('input[type="file"][name="files"]');
    if (!textarea || !fileInput) return;

    var strip = form.querySelector('.pasted-previews');
    var errorLine = form.querySelector('.paste-error');
    var urlCache = new Map(); // File -> object URL for the preview thumbnail

    function currentFiles() {
      var out = [];
      for (var i = 0; i < fileInput.files.length; i++) out.push(fileInput.files[i]);
      return out;
    }

    function urlFor(file) {
      if (!urlCache.has(file)) urlCache.set(file, URL.createObjectURL(file));
      return urlCache.get(file);
    }

    function showError(msg) {
      if (!errorLine) return;
      errorLine.textContent = msg;
      errorLine.hidden = false;
    }

    function chip(file, index) {
      var el = document.createElement('div');
      el.className = 'pasted-chip';

      var img = document.createElement('img');
      img.src = urlFor(file);
      img.alt = '';

      var name = document.createElement('span');
      name.className = 'pasted-chip-name';
      name.textContent = (file.name || 'pasted image') + ' (' + formatBytes(file.size) + ')';

      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'pasted-chip-remove';
      btn.setAttribute('aria-label', 'Remove ' + (file.name || 'pasted image'));
      btn.textContent = '\u00D7';
      btn.addEventListener('click', function () {
        if (urlCache.has(file)) {
          URL.revokeObjectURL(urlCache.get(file));
          urlCache.delete(file);
        }
        var rest = currentFiles();
        rest.splice(index, 1);
        setFiles(rest);
      });

      el.appendChild(img);
      el.appendChild(name);
      el.appendChild(btn);
      return el;
    }

    function render() {
      if (!strip) return;
      while (strip.firstChild) strip.removeChild(strip.firstChild);
      for (var i = 0; i < fileInput.files.length; i++) {
        strip.appendChild(chip(fileInput.files[i], i));
      }
      strip.hidden = fileInput.files.length === 0;
    }

    // FileLists are immutable; rebuild one via DataTransfer and reassign.
    function setFiles(files) {
      var dt = new DataTransfer();
      for (var i = 0; i < files.length; i++) dt.items.add(files[i]);
      fileInput.files = dt.files;
      render();
    }

    function addFiles(list) {
      var files = currentFiles();
      var added = 0;
      var rejected = 0;
      for (var i = 0; i < list.length; i++) {
        var f = list[i];
        if (!f || f.size === 0) continue;
        if (f.size > MAX_BYTES) {
          rejected++;
          showError('"' + (f.name || 'pasted image') + '" exceeds the 50MB limit.');
          continue;
        }
        if (!f.name) {
          // Clipboard images carry an empty filename.
          f = new File([f], 'clipboard-image-' + (files.length + 1) + '.png', {
            type: f.type || 'image/png',
          });
        }
        files.push(f);
        added++;
      }
      if (added > 0) {
        setFiles(files);
        if (rejected === 0 && errorLine) errorLine.hidden = true;
      }
    }

    // Paste: extract image items from the clipboard; let plain-text
    // pastes fall through untouched.
    textarea.addEventListener('paste', function (event) {
      var clipboard = event.clipboardData || window.clipboardData;
      if (!clipboard || !clipboard.items) return;
      var images = [];
      for (var i = 0; i < clipboard.items.length; i++) {
        var item = clipboard.items[i];
        if (item.kind === 'file' && item.type.indexOf('image/') === 0) {
          var f = item.getAsFile();
          if (f) images.push(f);
        }
      }
      if (images.length === 0) return;
      event.preventDefault(); // otherwise the image lands as stray text
      addFiles(images);
    });

    // Drag-and-drop on the whole form: same pipeline as paste.
    form.addEventListener('dragover', function (event) {
      if (event.dataTransfer && event.dataTransfer.types &&
          Array.prototype.indexOf.call(event.dataTransfer.types, 'Files') !== -1) {
        event.preventDefault();
      }
    });
    form.addEventListener('drop', function (event) {
      if (!event.dataTransfer || event.dataTransfer.files.length === 0) return;
      event.preventDefault();
      addFiles(event.dataTransfer.files);
    });

    // HTMX replies call form.reset() after a successful request; the
    // reset event fires BEFORE the fields clear, so defer the cleanup.
    form.addEventListener('reset', function () {
      setTimeout(function () {
        urlCache.forEach(function (url) { URL.revokeObjectURL(url); });
        urlCache.clear();
        render();
      }, 0);
    });
  }

  // Emoji picker: the "+" on a post's reaction bar toggles that bar's
  // picker (a server-rendered grid inside the bar). Document-level
  // delegation, so bars appended by HTMX work without rebinding. Clicking
  // outside, or pressing Escape, closes open pickers; picking an emoji
  // replaces the whole bar (hx-swap="outerHTML"), closing it for free.
  function bindPickers() {
    function closeAll() {
      var open = document.querySelectorAll('.emoji-picker:not(.is-hidden)');
      for (var i = 0; i < open.length; i++) {
        open[i].classList.add('is-hidden');
        var btn = open[i].parentElement.querySelector('.reaction-add');
        if (btn) btn.setAttribute('aria-expanded', 'false');
      }
    }
    document.addEventListener('click', function (event) {
      var wrap = event.target.closest
        ? event.target.closest('.reaction-picker-wrap')
        : null;
      if (wrap) {
        var picker = wrap.querySelector('.emoji-picker');
        var btn = wrap.querySelector('.reaction-add');
        var nowOpen = picker.classList.toggle('is-hidden') === false;
        if (btn) btn.setAttribute('aria-expanded', String(nowOpen));
        return;
      }
      closeAll();
    });
    document.addEventListener('keydown', function (event) {
      if (event.key === 'Escape') closeAll();
    });
  }

  // Toast auto-dismiss: flash messages fade in bottom-right and go away on
  // their own. Hovering or focusing a toast pauses the timer so a long error
  // can still be read; the × button dismisses immediately.
  function bindToasts() {
    var toasts = document.querySelectorAll('.toast');
    for (var i = 0; i < toasts.length; i++) {
      (function (toast) {
        var timer = null;

        function dismiss() {
          if (timer) clearTimeout(timer);
          toast.classList.add('is-leaving');
          setTimeout(function () {
            if (toast.parentNode) toast.parentNode.removeChild(toast);
          }, 250);
        }

        function schedule() {
          if (timer) clearTimeout(timer);
          timer = setTimeout(dismiss, 6000);
        }

        var close = toast.querySelector('.toast-close');
        if (close) close.addEventListener('click', dismiss);
        toast.addEventListener('mouseenter', function () {
          if (timer) clearTimeout(timer);
        });
        toast.addEventListener('mouseleave', schedule);
        toast.addEventListener('focusin', function () {
          if (timer) clearTimeout(timer);
        });
        toast.addEventListener('focusout', schedule);

        schedule();
      })(toasts[i]);
    }
  }

  // Image lightbox: attachment thumbnails open in a native <dialog> instead
  // of navigating to the raw upload. Delegated so HTMX-added posts work too.
  // Inside a [data-lightbox-group] the dialog browses the whole group (a
  // thread's images, in post order): on-screen arrows, ←/→ and Home/End, and
  // horizontal swipe. Links outside a group keep the one-image behaviour.
  function bindLightbox() {
    var dialog = document.getElementById('lightbox');
    if (!dialog) return;

    var img = dialog.querySelector('.lightbox-img');
    var close = dialog.querySelector('.lightbox-close');
    var prevBtn = dialog.querySelector('.lightbox-prev');
    var nextBtn = dialog.querySelector('.lightbox-next');
    var meta = dialog.querySelector('.lightbox-meta');
    var counter = dialog.querySelector('.lightbox-counter');
    var nameEl = dialog.querySelector('.lightbox-name');

    var items = [];   // { full, thumb, alt }
    var index = 0;
    var token = 0;    // guards the thumbnail -> original swap
    var drag = null;  // { x, y } while a pointer is down

    // Group scope is an explicit attribute so a .lightbox-link added
    // somewhere new can never accidentally join a thread's image set.
    function linksFor(link) {
      var group = link.closest ? link.closest('[data-lightbox-group]') : null;
      if (!group) return [link];
      return Array.prototype.slice.call(group.querySelectorAll('.lightbox-link'));
    }

    function itemFor(link) {
      var thumb = link.querySelector('img');
      return {
        full: link.getAttribute('href'),
        thumb: thumb ? (thumb.currentSrc || thumb.src) : '',
        alt: thumb ? thumb.alt : ''
      };
    }

    function render() {
      var item = items[index];
      if (!item) return;
      var mine = ++token;

      img.alt = item.alt;
      // The grid already cached this derivative, so it paints immediately.
      img.src = item.thumb || item.full;

      // Upgrade to full resolution only after it has actually loaded: swapping
      // a src the browser has cached is a repaint, not a reload, so there is
      // no blank frame. The token check drops stale loads after fast
      // navigation (they still warm the cache for that image).
      if (item.thumb && item.thumb !== item.full) {
        var full = new Image();
        full.onload = function () {
          if (mine === token) img.src = item.full;
        };
        full.src = item.full;
      }

      var multiple = items.length > 1;
      if (counter) counter.textContent = (index + 1) + ' / ' + items.length;
      if (nameEl) nameEl.textContent = item.alt;
      if (prevBtn) { prevBtn.disabled = index === 0; prevBtn.hidden = !multiple; }
      if (nextBtn) { nextBtn.disabled = index === items.length - 1; nextBtn.hidden = !multiple; }
      if (meta) meta.hidden = !multiple;
    }

    function go(delta) {
      var next = index + delta;
      if (next < 0 || next >= items.length || next === index) return;
      index = next;
      render();
    }

    function open(link) {
      var links = linksFor(link);
      items = links.map(itemFor);
      index = Math.max(0, links.indexOf(link));
      if (dialog.showModal) dialog.showModal();
      render();
    }

    document.addEventListener('click', function (event) {
      var link = event.target.closest ? event.target.closest('.lightbox-link') : null;
      if (!link || !img) return;
      event.preventDefault();
      open(link);
    });

    // Clicking the dimmed backdrop closes the dialog; clicks on the image
    // itself do not (the image is not a child of the dialog's padding box
    // click target, but guard anyway).
    dialog.addEventListener('click', function (event) {
      if (event.target === dialog) dialog.close();
    });

    if (close) close.addEventListener('click', function () { dialog.close(); });
    if (prevBtn) prevBtn.addEventListener('click', function () { go(-1); });
    if (nextBtn) nextBtn.addEventListener('click', function () { go(1); });

    // Escape is deliberately not handled: <dialog> closes on it natively.
    dialog.addEventListener('keydown', function (event) {
      if (event.altKey || event.ctrlKey || event.metaKey) return;
      if (event.key === 'ArrowRight') {
        event.preventDefault();
        go(1);
      } else if (event.key === 'ArrowLeft') {
        event.preventDefault();
        go(-1);
      } else if (event.key === 'Home') {
        event.preventDefault();
        index = 0;
        render();
      } else if (event.key === 'End') {
        event.preventDefault();
        index = items.length - 1;
        render();
      }
    });

    // Swipe. touch-action: pan-y on the image lets horizontal drags through as
    // pointer events; vertical drags stay native and are ignored here.
    dialog.addEventListener('pointerdown', function (event) {
      if (event.target.closest && event.target.closest('.lightbox-nav, .lightbox-close')) {
        drag = null;
        return;
      }
      drag = { x: event.clientX, y: event.clientY };
    });

    dialog.addEventListener('pointerup', function (event) {
      if (!drag) return;
      var dx = event.clientX - drag.x;
      var dy = event.clientY - drag.y;
      drag = null;
      if (Math.abs(dx) < 40 || Math.abs(dx) <= Math.abs(dy) * 1.5) return;
      go(dx < 0 ? 1 : -1);
    });

    dialog.addEventListener('pointercancel', function () { drag = null; });

    dialog.addEventListener('close', function () {
      items = [];
      index = 0;
      token++;
      drag = null;
      if (img) img.removeAttribute('src');
    });
  }

  // Ctrl/Cmd+Enter submits any composer form that has a body textarea.
  function bindShortcuts() {
    var forms = document.querySelectorAll('form');
    for (var i = 0; i < forms.length; i++) {
      (function (form) {
        var textarea = form.querySelector('textarea[name="body"]');
        if (!textarea) return;
        textarea.addEventListener('keydown', function (event) {
          if (event.key === 'Enter' && (event.ctrlKey || event.metaKey)) {
            event.preventDefault();
            if (form.requestSubmit) {
              form.requestSubmit();
            } else {
              form.submit();
            }
          }
        });
      })(forms[i]);
    }
  }

  // HTMX entry animations: new posts slide in, and the reaction bar that
  // replaces itself after a toggle pops the chip you just hit.
  function bindHtmxMotion() {
    document.addEventListener('htmx:load', function (event) {
      var el = event.detail && event.detail.elt;
      if (!el || !el.classList) return;
      if (el.classList.contains('post-card')) {
        el.classList.add('post-enter');
      }
      if (el.classList.contains('reaction-bar')) {
        var active = el.querySelector('.reaction-chip.is-active');
        if (active) active.classList.add('chip-pop');
      }
    });
  }

  // Light/dark toggle: flips the effective theme, writes the choice to a
  // cookie (read server-side for the next full page load), and keeps the
  // icon/label in sync. Without an explicit choice the CSS follows the OS.
  function bindThemeToggle() {
    var btn = document.getElementById('theme-toggle');
    if (!btn) return;
    var query = window.matchMedia ? window.matchMedia('(prefers-color-scheme: light)') : null;

    function effective() {
      var explicit = document.documentElement.getAttribute('data-theme');
      if (explicit === 'light' || explicit === 'dark') return explicit;
      return query && query.matches ? 'light' : 'dark';
    }

    function label() {
      var next = effective() === 'dark' ? 'light' : 'dark';
      btn.setAttribute('aria-label', 'Switch to ' + next + ' theme');
      btn.title = btn.getAttribute('aria-label');
    }

    btn.addEventListener('click', function () {
      var next = effective() === 'dark' ? 'light' : 'dark';
      document.documentElement.setAttribute('data-theme', next);
      document.cookie = 'disjoint_theme=' + next + '; Path=/; Max-Age=31536000; SameSite=Lax';
      label();
    });

    label();
  }

  // Old permalinks (stored quotes, bookmarks) use /threads/{id}#post-N
  // without ?from=. If that post is on a later page, ask the server once to
  // resolve the page. A canonical page URL always carries ?page=, so a
  // missing/deleted post cannot cause a redirect loop.
  function bindPostAnchorFallback() {
    var match = /^#post-(\d+)$/.exec(window.location.hash);
    if (!match) return;
    if (new URLSearchParams(window.location.search).has('page')) return;
    if (document.getElementById('post-' + match[1])) return;
    var container = document.getElementById('posts-container');
    if (!container) return;
    var threadID = container.getAttribute('data-thread-id');
    if (!threadID) return;
    window.location.replace('/threads/' + threadID + '?from=' + match[1] + '#post-' + match[1]);
  }

  // Bulma ships without JS. This is the vanilla burger toggle from the navbar
  // docs: flip is-active on the burger and the navbar-menu it targets, keeping
  // aria-expanded in sync. Moved out of layout.html so the page can run under
  // a script-src 'self' CSP.
  function bindNavbar() {
    var burgers = document.querySelectorAll('.navbar-burger');
    for (var i = 0; i < burgers.length; i++) {
      (function (el) {
        el.addEventListener('click', function () {
          var target = el.dataset.target;
          var menu = target ? document.getElementById(target) : null;
          el.classList.toggle('is-active');
          if (menu) menu.classList.toggle('is-active');
          el.setAttribute('aria-expanded', el.classList.contains('is-active') ? 'true' : 'false');
        });
      })(burgers[i]);
    }
  }

  // Confirmation prompts declared as data-confirm="..." (replaces inline
  // onsubmit handlers, which a script-src 'self' CSP blocks). Delegated so
  // forms added by HTMX are covered too.
  function bindConfirmSubmit() {
    document.addEventListener('submit', function (event) {
      var form = event.target;
      if (!form || !form.hasAttribute || !form.hasAttribute('data-confirm')) return;
      if (!window.confirm(form.getAttribute('data-confirm'))) {
        event.preventDefault();
        event.stopPropagation();
      }
    });
  }

  // Inline-edit toggles: a [data-toggle-form="#id"] button shows/hides the
  // form it points at and focuses its first field. Delegated, so it works on
  // HTMX-swapped content.
  function bindToggleForms() {
    document.addEventListener('click', function (event) {
      var btn = event.target.closest ? event.target.closest('[data-toggle-form]') : null;
      if (!btn) return;
      var form = document.querySelector(btn.getAttribute('data-toggle-form'));
      if (!form) return;
      form.classList.toggle('is-hidden');
      btn.classList.toggle('is-hidden');
      if (!form.classList.contains('is-hidden')) {
        var input = form.querySelector('input, select');
        if (input) {
          input.focus();
          if (input.select) input.select();
        }
      }
    });
  }

  // Composer preview: a [data-preview-toggle="#id"] button shows/hides the
  // panel it points at and asks HTMX to refresh the panel only when opening.
  // The panel carries hx-post="/preview" + hx-trigger="preview", so closing
  // never wastes a render. Delegated so it also works on swapped content.
  function bindComposerPreview() {
    document.addEventListener('click', function (event) {
      var btn = event.target.closest ? event.target.closest('[data-preview-toggle]') : null;
      if (!btn) return;
      var panel = document.querySelector(btn.getAttribute('data-preview-toggle'));
      if (!panel) return;

      var opening = panel.classList.contains('is-hidden');
      panel.classList.toggle('is-hidden', !opening);
      btn.setAttribute('aria-expanded', opening ? 'true' : 'false');
      btn.textContent = opening ? 'Hide preview' : 'Preview';

      if (opening && window.htmx) window.htmx.trigger(panel, 'preview');
    });
  }

  // Quote insertion: one delegated listener on #posts-container so it also
  // works on post cards appended by HTMX. The blockquote string built here is
  // pinned by TestQuoteBlockquoteFormat in internal/markdown. Moved out of
  // thread.html so the page can run under a script-src 'self' CSP.
  function bindQuoteButton() {
    var container = document.getElementById('posts-container');
    var textarea = document.getElementById('reply-body');
    if (!container || !textarea) return;
    var threadID = container.getAttribute('data-thread-id');

    // Soft cap on quoted post text (decision Q3). The attribution and
    // "view original" permalink lines are fixed overhead, always kept.
    var MAX_QUOTE_CHARS = 500;

    container.addEventListener('click', function (event) {
      var btn = event.target.closest ? event.target.closest('.quote-btn') : null;
      if (!btn) return;
      var card = btn.closest('.post-card');
      if (!card) return;

      var authorEl = card.querySelector('.post-author');
      var author = authorEl ? authorEl.textContent.trim() : 'unknown';
      var postID = card.id.replace(/^post-/, '');

      // Rendered body text. innerText (not textContent) turns block
      // boundaries and <br> into newlines; attachments live outside
      // .content, so they are excluded by construction.
      var content = card.querySelector('.content');
      var lines = (content ? content.innerText : '')
        .split('\n')
        .map(function (l) { return l.trim(); });
      while (lines.length && lines[0] === '') lines.shift();
      while (lines.length && lines[lines.length - 1] === '') lines.pop();
      if (lines.length === 0) lines = ['']; // keep the quote one blockquote

      // Apply the soft cap, then re-split for line prefixing. Cutting
      // mid-line leaves a partial last line; cutting on a line boundary
      // leaves the ellipsis on the previous line — both render fine.
      var text = lines.join('\n');
      if (text.length > MAX_QUOTE_CHARS) {
        text = text.slice(0, MAX_QUOTE_CHARS).replace(/\s+$/, '') + '\u2026';
      }
      lines = text.split('\n');

      // Every physical line is > -prefixed (blank lines as bare >) so the
      // whole quote is one blockquote after rendering.
      var quote = '> **' + author + ' wrote:**\n' +
        lines.map(function (l) { return l === '' ? '>' : '> ' + l; }).join('\n') +
        '\n>\n' +
        '> \u2014 [view original](/threads/' + threadID + '?from=' + postID + '#post-' + postID + ')';

      // Append after any existing reply text; never clobber it.
      var existing = textarea.value;
      var sep = existing === '' ? '' : (existing.slice(-2) === '\n\n' ? '' : existing.slice(-1) === '\n' ? '\n' : '\n\n');
      textarea.value = existing + sep + quote;
      textarea.focus();
      textarea.setSelectionRange(textarea.value.length, textarea.value.length);
      textarea.scrollIntoView({ behavior: 'smooth', block: 'center' });
    });
  }

  // HTMX lifecycle hooks that replace the former inline hx-on and [...]
  // trigger filters (both of which require eval, which the CSP forbids):
  //   - data-pause-hidden: skip polls while the tab is hidden.
  //   - data-reset-on-success: clear a composer after a successful request.
  function bindHtmxLifecycle() {
    document.addEventListener('htmx:beforeRequest', function (event) {
      if (!document.hidden) return;
      var elt = event.detail && event.detail.elt;
      if (elt && elt.hasAttribute && elt.hasAttribute('data-pause-hidden')) {
        event.preventDefault();
      }
    });
    document.addEventListener('htmx:afterRequest', function (event) {
      var detail = event.detail;
      if (!detail || !detail.successful) return;
      var elt = detail.elt;
      var form = elt && elt.tagName === 'FORM' ? elt : (elt && elt.closest ? elt.closest('form') : null);
      if (form && form.hasAttribute('data-reset-on-success')) form.reset();
    });
  }

  function init() {
    var forms = document.querySelectorAll('form');
    for (var i = 0; i < forms.length; i++) bindForm(forms[i]);
    bindPostAnchorFallback();
    bindPickers();
    bindToasts();
    bindLightbox();
    bindShortcuts();
    bindHtmxMotion();
    bindThemeToggle();
    bindNavbar();
    bindConfirmSubmit();
    bindToggleForms();
    bindComposerPreview();
    bindQuoteButton();
    bindHtmxLifecycle();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
