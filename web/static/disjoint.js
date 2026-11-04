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

  function init() {
    var forms = document.querySelectorAll('form');
    for (var i = 0; i < forms.length; i++) bindForm(forms[i]);
    bindPickers();
    bindToasts();
    bindLightbox();
    bindShortcuts();
    bindHtmxMotion();
    bindThemeToggle();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
