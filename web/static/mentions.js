// mentions.js — @username autocomplete for compose textareas.
//
// The server renders @mentions; this file only suggests usernames while
// typing. It mirrors the markdown parser's left-boundary rule so it never
// suggests inside an e-mail address or a glued word. Selecting a suggestion
// replaces the token with "@name " and then gets out of the way.
//
// Every compose box is a plain <textarea> in a Bulma .control wrapper. The
// list is positioned against that wrapper, not the caret (no mirror-div).

(function () {
  'use strict';

  var DEBOUNCE_MS = 120;
  var listCounter = 0;

  function isWordChar(ch) {
    return /[A-Za-z0-9_]/.test(ch);
  }

  // tokenAtCaret returns {start, end, text} for an @token ending exactly at
  // the caret, or null. The left boundary matches internal/markdown/mention.go:
  // start-of-text or a non-word character before the @.
  function tokenAtCaret(textarea) {
    var caret = textarea.selectionStart;
    if (typeof caret !== 'number') return null;

    var before = textarea.value.slice(0, caret);
    var match = /@([A-Za-z0-9]{0,32})$/.exec(before);
    if (!match) return null;

    var start = caret - match[0].length;
    if (start > 0 && isWordChar(before.charAt(start - 1))) return null;

    return { start: start, end: caret, text: match[1] };
  }

  function bind(textarea) {
    if (textarea.readOnly || textarea.disabled) return;

    var control = textarea.closest ? textarea.closest('.control') : null;
    if (!control) control = textarea.parentElement;
    if (!control) return;

    control.classList.add('mention-suggestions-host');

    var list = document.createElement('ul');
    list.className = 'mention-suggestions';
    list.id = 'mention-suggestions-' + (++listCounter);
    list.setAttribute('role', 'listbox');
    list.hidden = true;
    control.appendChild(list);

    var users = [];
    var active = -1;
    var timer = null;
    var controller = null;
    var seq = 0;

    function close() {
      if (timer) {
        clearTimeout(timer);
        timer = null;
      }
      if (controller) {
        controller.abort();
        controller = null;
      }
      seq++;
      list.hidden = true;
      while (list.firstChild) list.removeChild(list.firstChild);
      users = [];
      active = -1;
      textarea.removeAttribute('role');
      textarea.removeAttribute('aria-expanded');
      textarea.removeAttribute('aria-controls');
      textarea.removeAttribute('aria-activedescendant');
      textarea.removeAttribute('aria-autocomplete');
    }

    function setActive(index) {
      if (!users.length) return;
      active = (index + users.length) % users.length;
      for (var i = 0; i < list.children.length; i++) {
        var on = i === active;
        list.children[i].classList.toggle('is-active', on);
        list.children[i].setAttribute('aria-selected', on ? 'true' : 'false');
      }
      textarea.setAttribute('aria-activedescendant', list.children[active].id);
    }

    function render(items) {
      users = items;
      while (list.firstChild) list.removeChild(list.firstChild);
      if (!users.length) {
        close();
        return;
      }

      for (var i = 0; i < users.length; i++) {
        (function (index) {
          var user = users[index];
          var item = document.createElement('li');
          item.id = list.id + '-option-' + index;
          item.setAttribute('role', 'option');
          item.setAttribute('aria-selected', 'false');

          var avatar = document.createElement('img');
          avatar.className = 'mention-avatar';
          avatar.src = user.avatar;
          avatar.alt = '';
          avatar.loading = 'lazy';

          var name = document.createElement('span');
          name.className = 'mention-suggestion-name';
          name.textContent = '@' + user.name;

          item.appendChild(avatar);
          item.appendChild(name);
          item.addEventListener('pointerdown', function (event) {
            // Prevent the textarea from blurring before the selection lands;
            // this covers mouse and touch in one handler.
            event.preventDefault();
            choose(index);
          });
          item.addEventListener('click', function (event) {
            event.preventDefault();
            choose(index);
          });
          list.appendChild(item);
        })(i);
      }

      list.hidden = false;
      textarea.setAttribute('role', 'combobox');
      textarea.setAttribute('aria-expanded', 'true');
      textarea.setAttribute('aria-controls', list.id);
      textarea.setAttribute('aria-autocomplete', 'list');
      setActive(0);
    }

    function choose(index) {
      if (!users[index]) return;
      var current = tokenAtCaret(textarea);
      if (!current) {
        close();
        return;
      }

      var insertion = '@' + users[index].name + ' ';
      if (typeof textarea.setRangeText === 'function') {
        textarea.setRangeText(insertion, current.start, current.end, 'end');
      } else {
        var value = textarea.value;
        textarea.value = value.slice(0, current.start) + insertion + value.slice(current.end);
        var caret = current.start + insertion.length;
        textarea.selectionStart = caret;
        textarea.selectionEnd = caret;
      }

      close();
      textarea.focus();
      textarea.dispatchEvent(new Event('input', { bubbles: true }));
    }

    function fetchSuggestions(query) {
      if (controller) controller.abort();
      controller = new AbortController();
      var requestSeq = ++seq;

      fetch('/api/mentions?q=' + encodeURIComponent(query), {
        credentials: 'same-origin',
        signal: controller.signal,
        headers: { Accept: 'application/json' },
      })
        .then(function (response) {
          if (!response.ok) throw new Error('HTTP ' + response.status);
          return response.json();
        })
        .then(function (data) {
          if (requestSeq !== seq) return;
          controller = null;
          var current = tokenAtCaret(textarea);
          if (!current || current.text !== query) return;
          render(Array.isArray(data.users) ? data.users : []);
        })
        .catch(function (error) {
          if (error && error.name === 'AbortError') return;
          if (requestSeq !== seq) return;
          controller = null;
          close();
        });
    }

    function schedule() {
      if (timer) {
        clearTimeout(timer);
        timer = null;
      }

      var token = tokenAtCaret(textarea);
      if (!token) {
        close();
        return;
      }

      timer = setTimeout(function () {
        timer = null;
        fetchSuggestions(token.text);
      }, DEBOUNCE_MS);
    }

    textarea.addEventListener('input', schedule);
    textarea.addEventListener('click', schedule);
    textarea.addEventListener('keyup', function (event) {
      // These keys navigate/commit an open list; do not refetch and reset the
      // highlighted row behind the user's back.
      if (!list.hidden && ['ArrowDown', 'ArrowUp', 'Enter', 'Tab', 'Escape'].indexOf(event.key) !== -1) return;
      schedule();
    });

    textarea.addEventListener('keydown', function (event) {
      if (list.hidden) return;
      // Ctrl/Cmd+Enter is the existing "submit post" shortcut; never consume
      // it as a mention selection.
      if (event.ctrlKey || event.metaKey || event.altKey) return;

      if (event.key === 'ArrowDown') {
        event.preventDefault();
        setActive(active + 1);
      } else if (event.key === 'ArrowUp') {
        event.preventDefault();
        setActive(active - 1);
      } else if (event.key === 'Enter' || event.key === 'Tab') {
        if (users.length) {
          event.preventDefault();
          choose(active >= 0 ? active : 0);
        }
      } else if (event.key === 'Escape') {
        event.preventDefault();
        close();
      }
    });

    textarea.addEventListener('blur', function () {
      // Delay so a click on an option can land (mousedown prevents this blur,
      // but touch/keyboard-assisted clicks can still blur first).
      setTimeout(function () {
        if (document.activeElement !== textarea) close();
      }, 150);
    });

    if (textarea.form) {
      textarea.form.addEventListener('reset', close);
    }
  }

  function init() {
    var textareas = document.querySelectorAll('textarea');
    for (var i = 0; i < textareas.length; i++) {
      bind(textareas[i]);
    }
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
