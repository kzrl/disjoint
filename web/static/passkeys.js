// Passkey (WebAuthn) client glue. The server owns the ceremony state; this
// file only converts base64url strings from the JSON options into the
// ArrayBuffers the browser API expects, and the freshly created credential back
// into JSON.
//
// Loaded only on /login and /account (see the "scripts" block).
(function () {
  "use strict";

  function supported() {
    return !!(window.PublicKeyCredential && navigator.credentials && navigator.credentials.create);
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

  // The server hands back the PublicKeyCredentialCreationOptions/
  // PublicKeyCredentialRequestOptions with every binary member base64url
  // encoded, exactly as JSON serialization requires.
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
      return res.json().catch(function () { return {}; }).then(function (data) {
        if (!res.ok) {
          throw new Error(data.error || "Request failed");
        }
        return data;
      });
    });
  }

  function status(el, message, isError) {
    if (!el) return;
    el.hidden = false;
    el.textContent = message;
    el.classList.toggle("has-text-danger", !!isError);
    el.classList.toggle("has-text-success", !isError);
  }

  function friendlyError(err) {
    if (err && err.name === "NotAllowedError") {
      return "Passkey prompt was dismissed or timed out.";
    }
    if (err && err.name === "InvalidStateError") {
      return "That passkey is already registered to this account.";
    }
    return (err && err.message) || "Passkey operation failed.";
  }

  // --- Login page ---------------------------------------------------------

  function bindLogin() {
    var button = document.getElementById("passkey-login-btn");
    if (!button) return;
    var statusEl = document.getElementById("passkey-login-status");
    if (!supported()) {
      // Password form remains; nothing to show.
      return;
    }
    button.hidden = false;

    button.addEventListener("click", function () {
      button.disabled = true;
      status("", "", false);
      if (statusEl) statusEl.hidden = true;
      postJSON("/login/passkey/begin", {})
        .then(function (data) {
          return navigator.credentials.get({ publicKey: decodeRequestOptions(data) });
        })
        .then(function (cred) {
          return postJSON("/login/passkey/finish", encodeAssertion(cred));
        })
        .then(function (data) {
          window.location.assign(data.redirect || "/");
        })
        .catch(function (err) {
          status(statusEl, friendlyError(err), true);
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

    if (!supported()) {
      form.hidden = true;
      status(statusEl, "This browser does not support passkeys.", true);
      return;
    }

    form.addEventListener("submit", function (event) {
      event.preventDefault();
      button.disabled = true;
      status(statusEl, "Waiting for your authenticator…", false);
      postJSON("/account/passkeys/begin", { password: passwordInput.value })
        .then(function (data) {
          return navigator.credentials.create({ publicKey: decodeCreationOptions(data) });
        })
        .then(function (cred) {
          return postJSON("/account/passkeys/finish", encodeRegistration(cred));
        })
        .then(function () {
          // Reload so the new row and its server-assigned name appear.
          window.location.reload();
        })
        .catch(function (err) {
          status(statusEl, friendlyError(err), true);
          button.disabled = false;
        });
    });
  }

  bindLogin();
  bindAccount();
})();
