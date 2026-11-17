// The preference page (task 6.4): it saves the person's choices through
// the public preferences API. No framework, no inline script: the
// page's CSP allows this file only.
(function () {
  "use strict";
  var root = document.getElementById("preferences");
  var form = document.getElementById("choices");
  var status = document.getElementById("status");
  if (!root || !form) { return; }
  var url = "/v1/contacts/preferences/" + encodeURIComponent(root.dataset.token);
  function say(text) { status.textContent = text; }
  function key() {
    if (window.crypto && window.crypto.randomUUID) { return window.crypto.randomUUID(); }
    return String(Date.now()) + "-" + String(Math.random()).slice(2);
  }
  form.addEventListener("submit", function (e) {
    e.preventDefault();
    var button = form.querySelector("button");
    var channels = {};
    Array.prototype.forEach.call(form.querySelectorAll("input[type=checkbox]"), function (box) {
      channels[box.name] = box.checked;
    });
    button.disabled = true;
    say("Saving…");
    fetch(url, {
      method: "PUT",
      headers: { "Content-Type": "application/json", "Idempotency-Key": key() },
      body: JSON.stringify({ channels: channels })
    })
      .then(function (r) { return r.json().then(function (b) { return { ok: r.ok, body: b }; }); })
      .then(function (res) {
        button.disabled = false;
        if (!res.ok) { say(res.body.detail || res.body.title || "That didn't work."); return; }
        res.body.channels.forEach(function (c) {
          var box = form.querySelector("input[name=" + c.channel + "]");
          if (box) { box.checked = c.receiving; }
        });
        say("Saved.");
      })
      .catch(function () { button.disabled = false; say("That didn't work. Check your connection and try again."); });
  });
})();
