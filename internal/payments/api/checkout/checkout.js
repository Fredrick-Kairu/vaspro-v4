// The hosted checkout page (task 4.8): it starts an STK push through
// the public checkout API and polls the payment until it ends. No
// framework, no inline script: the page's CSP allows this file only.
(function () {
  "use strict";
  var root = document.getElementById("checkout");
  var form = document.getElementById("pay");
  var status = document.getElementById("status");
  if (!root || !form) { return; }
  var base = "/v1/payments/checkout/" + encodeURIComponent(root.dataset.token);
  var messages = {
    pending: "Check your phone and enter your M-Pesa PIN.",
    succeeded: "Paid. Thank you.",
    cancelled: "You cancelled the prompt. You can try again.",
    failed: "The payment didn't go through. You can try again.",
    expired: "The prompt timed out. You can try again."
  };
  function say(text) { status.textContent = text; }
  function key() {
    if (window.crypto && window.crypto.randomUUID) { return window.crypto.randomUUID(); }
    return String(Date.now()) + "-" + String(Math.random()).slice(2);
  }
  function poll(id, tries) {
    fetch(base + "/payments/" + encodeURIComponent(id))
      .then(function (r) { return r.json(); })
      .then(function (p) {
        say(messages[p.status] || "Waiting for M-Pesa…");
        if (p.status === "succeeded") { form.remove(); return; }
        if ((p.status === "pending" || p.status === "created") && tries < 60) {
          setTimeout(function () { poll(id, tries + 1); }, 2000);
        } else { form.querySelector("button").disabled = false; }
      })
      .catch(function () { say("We couldn't check the payment. Refresh to see if it went through."); });
  }
  form.addEventListener("submit", function (e) {
    e.preventDefault();
    var button = form.querySelector("button");
    button.disabled = true;
    say("Sending the prompt…");
    fetch(base + "/payments", {
      method: "POST",
      headers: { "Content-Type": "application/json", "Idempotency-Key": key() },
      body: JSON.stringify({ payer_phone: form.phone.value.trim() })
    })
      .then(function (r) { return r.json().then(function (b) { return { ok: r.ok, body: b }; }); })
      .then(function (res) {
        if (!res.ok) { say(res.body.detail || res.body.title || "That didn't work."); button.disabled = false; return; }
        say(messages[res.body.status] || "Waiting for M-Pesa…");
        poll(res.body.payment_id, 0);
      })
      .catch(function () { say("That didn't work. Check your connection and try again."); button.disabled = false; });
  });
})();
