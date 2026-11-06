// Passkey (WebAuthn) client glue. The server owns the ceremony state; this
// file only converts base64url strings from the JSON options into the
// ArrayBuffers the browser API expects, and the freshly created credential back
// into JSON.
//
// Loaded only on /login and /account (see the "scripts" block).
//
// Safari (and therefore every iOS browser) requires navigator.credentials.* to
// be called from the same task as the user gesture. A `fetch()` for the
// ceremony options before the call loses that activation, so the flow is
// two-phase: the first tap fetches and stores the options, the second tap calls
// WebAuthn synchronously and then posts the result. Chrome/Firefox work with
// the same two taps, so there is one code path everywhere.
(function () {
  "use strict";

  var PENDING_TTL_MS = 4 * 60 * 1000; // ceremonies expire after 5 minutes server-side

  function supported() {
    return !!(window.PublicKeyCredential && navigator.credentials &&
      navigator.credentials.create && navigator.credentials.get);
  }

  function b64urlToBuf(value) {
    var padding = "=".repeat((4 - (value.length % 4)) % 4);
    var base64 = (value + padding).replace(/-/g, "+").replace(/_/g, "/");
    var raw = atob(base64);
    var out = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; i++) out[i] = raw.charCodeAt(i);
    return out.buffer;
  }

  function bufToB64url(buf) {
    var bytes = new Uint8Array(buf);
    var raw = "";
    for (var i = 0; i < bytes.length; i++) raw += String.fromCharCode(bytes[i]);
    return btoa(raw).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  }

  function decodeCreationOptions(options) {
    var pk = options.publicKey || options;
    pk.challenge = b64urlToBuf(pk.challenge);
    if (pk.user && pk.user.id) pk.user.id = b64urlToBuf(pk.user.id);
    (pk.excludeCredentials || []).forEach(function (c) {
      if (c && c.id) c.id = b64urlToBuf(c.id);
    });
    return pk;
  }

  function decodeRequestOptions(options) {
    var pk = options.publicKey || options;
    pk.challenge = b64urlToBuf(pk.challenge);
    (pk.allowCredentials || []).forEach(function (c) {
      if (c && c.id) c.id = b64urlToBuf(c.id);
    });
    return pk;
  }

  function encodeRegistration(cred) {
    var response = cred.response;
    var out = {
      id: cred.id,
      rawId: bufToB64url(cred.rawId),
      type: cred.type,
      response: {
        clientDataJSON: bufToB64url(response.clientDataJSON),
        attestationObject: bufToB64url(response.attestationObject)
      },
      clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {}
    };
    if (response.getTransports) {
      out.response.transports = response.getTransports();
    }
    if (cred.authenticatorAttachment) {
      out.authenticatorAttachment = cred.authenticatorAttachment;
    }
    return out;
  }

  function encodeAssertion(cred) {
    var response = cred.response;
    return {
      id: cred.id,
      rawId: bufToB64url(cred.rawId),
      type: cred.type,
      response: {
        clientDataJSON: bufToB64url(response.clientDataJSON),
        authenticatorData: bufToB64url(response.authenticatorData),
        signature: bufToB64url(response.signature),
        userHandle: response.userHandle ? bufToB64url(response.userHandle) : null
      },
      clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {}
    };
  }

  function postJSON(url, body) {
    return fetch(url, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body || {})
    }).then(function (res) {
      return res.text().then(function (text) {
        var data = {};
        try { data = text ? JSON.parse(text) : {}; } catch (e) { /* non-JSON error page */ }
        if (!res.ok) {
          var err = new Error(data.error || ("Request failed (" + res.status + ")"));
          err.status = res.status;
          throw err;
        }
        return data;
      });
    }, function () {
      throw new Error("Network error. Check your connection and try again.");
    });
  }

  function status(el, message, kind) {
    if (!el) return;
    el.hidden = false;
    el.textContent = message;
    el.classList.toggle("has-text-danger", kind === "error");
    el.classList.toggle("has-text-success", kind === "ok");
  }

  function messageFor(err) {
    if (!err) return "Passkey operation failed.";
    if (err.name === "NotAllowedError") {
      return "The passkey prompt was cancelled or timed out. Try again.";
    }
    if (err.name === "InvalidStateError") {
      return "That passkey is already registered to this account.";
    }
    if (err.name === "NotSupportedError" || err.name === "SecurityError") {
      return "This browser cannot create a passkey for this site. Passkeys need HTTPS and a recent browser.";
    }
    return err.message || "Passkey operation failed.";
  }

  function now() { return Date.now(); }

  // --- Login page ---------------------------------------------------------

  function bindLogin() {
    var button = document.getElementById("passkey-login-btn");
    if (!button) return;
    var statusEl = document.getElementById("passkey-login-status");
    if (!supported() || window.isSecureContext === false) return; // password form is the fallback
    button.hidden = false;

    var pending = null; // { publicKey, token, at }
    var pendingValid = function () {
      return pending && pending.publicKey && (now() - pending.at) < PENDING_TTL_MS;
    };

    // Prefetch assertion options so the tap can call WebAuthn synchronously
    // (required by Safari/iOS). Failure is silent: tapping still retries.
    postJSON("/login/passkey/begin", {}).then(function (data) {
      pending = { publicKey: data.publicKey, token: data.token, at: now() };
    }).catch(function () { /* retried on tap */ });

    function fetchOptions() {
      return postJSON("/login/passkey/begin", {}).then(function (data) {
        pending = { publicKey: data.publicKey, token: data.token, at: now() };
        return pending;
      });
    }

    function finish(cred) {
      status(statusEl, "Signing you in…", "ok");
      postJSON("/login/passkey/finish", encodeAssertion(cred))
        .then(function (data) {
          window.location.assign(data.redirect || "/");
        })
        .catch(function (err) {
          pending = null;
          status(statusEl, messageFor(err), "error");
          button.disabled = false;
        });
    }

    button.addEventListener("click", function () {
      button.disabled = true;
      status(statusEl, "", "ok");
      if (statusEl) statusEl.hidden = true;

      // Safari-safe path: WebAuthn is called before any fetch in this task.
      if (pendingValid()) {
        var options = decodeRequestOptions(pending);
        var p = navigator.credentials.get({ publicKey: options });
        status(statusEl, "Confirm with your passkey…", "ok");
        p.then(finish).catch(function (err) {
          pending = null;
          status(statusEl, messageFor(err), "error");
          button.disabled = false;
        });
        return;
      }

      // Chrome/Firefox are happy with a fetch-then-get; Safari users get a
      // second tap if the prefetch did not finish in time.
      status(statusEl, "Preparing sign-in…", "ok");
      fetchOptions().then(function () {
        button.disabled = false;
        status(statusEl, "Tap “Sign in with a passkey” again to continue.", "ok");
      }).catch(function (err) {
        status(statusEl, messageFor(err), "error");
        button.disabled = false;
      });
    });
  }

  // --- Account page -------------------------------------------------------

  function bindAccount() {
    var form = document.getElementById("add-passkey-form");
    if (!form) return;
    var passwordInput = document.getElementById("passkey_password");
    var button = document.getElementById("add-passkey-btn");
    var statusEl = document.getElementById("passkey-status");

    if (!supported() || window.isSecureContext === false) {
      form.hidden = true;
      status(statusEl, "This browser cannot create passkeys. They need HTTPS and a recent browser.", "error");
      return;
    }

    var pending = null; // { publicKey, token, at }
    var pendingValid = function () {
      return pending && pending.publicKey && (now() - pending.at) < PENDING_TTL_MS;
    };
    var resetButton = function (label) {
      button.disabled = false;
      button.textContent = label;
    };

    function finish(cred) {
      status(statusEl, "Saving passkey…", "ok");
      button.disabled = true;
      postJSON("/account/passkeys/finish", encodeRegistration(cred))
        .then(function () {
          status(statusEl, "Passkey saved. Reloading…", "ok");
          window.location.reload();
        })
        .catch(function (err) {
          pending = null;
          resetButton("Add passkey");
          status(statusEl, messageFor(err), "error");
        });
    }

    form.addEventListener("submit", function (event) {
      event.preventDefault();

      // Phase 2: the options are already fetched, so the WebAuthn call happens
      // synchronously inside this tap — the only form iOS Safari accepts.
      if (pendingValid()) {
        var options = decodeCreationOptions(pending);
        status(statusEl, "Confirm with Face ID, Touch ID, or your security key…", "ok");
        button.disabled = true;
        var p;
        try {
          p = navigator.credentials.create({ publicKey: options });
        } catch (err) {
          pending = null;
          resetButton("Add passkey");
          status(statusEl, messageFor(err), "error");
          return;
        }
        p.then(finish).catch(function (err) {
          pending = null;
          resetButton("Add passkey");
          status(statusEl, messageFor(err), "error");
        });
        return;
      }

      // Phase 1: confirm the password and fetch the ceremony options.
      if (!passwordInput || passwordInput.value === "") {
        status(statusEl, "Enter your current password first.", "error");
        if (passwordInput) passwordInput.focus();
        return;
      }
      button.disabled = true;
      status(statusEl, "Checking your password…", "ok");
      postJSON("/account/passkeys/begin", { password: passwordInput.value })
        .then(function (data) {
          pending = { publicKey: data.publicKey, token: data.token, at: now() };
          button.textContent = "Create passkey";
          button.disabled = false;
          status(statusEl, "Password confirmed. Tap “Create passkey” to finish.", "ok");
          if (passwordInput) passwordInput.value = "";
        })
        .catch(function (err) {
          resetButton("Add passkey");
          status(statusEl, messageFor(err), "error");
        });
    });
  }

  function init() {
    try {
      bindLogin();
      bindAccount();
    } catch (err) {
      // Never leave the user with a silent, dead button.
      var el = document.getElementById("passkey-status") ||
        document.getElementById("passkey-login-status");
      if (el) status(el, "Passkeys could not start on this page: " + messageFor(err), "error");
    }
  }

  // The script lives in <head> (with defer) but this guard makes the file safe
  // to include without it: binding before the DOM exists silently did nothing,
  // which is exactly the "button does nothing" failure.
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", init);
  } else {
    init();
  }
})();
