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

  function init() {
    var forms = document.querySelectorAll('form');
    for (var i = 0; i < forms.length; i++) bindForm(forms[i]);
    bindPickers();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
