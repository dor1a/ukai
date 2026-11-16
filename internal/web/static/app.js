// Clear the URL field after a download was queued. The server marks a
// rejected submission with an X-Ukai-Error header, and then the field keeps
// what was typed.
document.addEventListener("htmx:afterRequest", function (event) {
  var form = event.target;
  if (!form.matches || !form.matches("form.submit")) {
    return;
  }
  var xhr = event.detail.xhr;
  if (event.detail.successful && xhr && !xhr.getResponseHeader("X-Ukai-Error")) {
    form.reset();
    // After a partly queued batch the box was swapped in holding the URLs
    // left, which reset() would bring back, so empty it outright.
    var box = form.querySelector("textarea");
    if (box) {
      box.value = "";
      box.defaultValue = "";
    }
  }
});

// Enter in the URL box queues the download, as the single-line field did.
// Shift+Enter starts a new line for another URL.
document.addEventListener("keydown", function (event) {
  var box = event.target;
  if (event.key !== "Enter" || event.shiftKey || event.isComposing || !box.matches || !box.matches("form.submit textarea")) {
    return;
  }
  event.preventDefault();
  box.form.requestSubmit();
});

// Keep the job list's own refresh from replacing it while something in it is
// playing: the swap would move the player and could stop it. The refresh is
// skipped before it is sent, and also when playback started while it was on
// its way. The next tick tries again.
function playing(list) {
  var media = list.querySelectorAll("audio, video");
  for (var i = 0; i < media.length; i++) {
    if (!media[i].paused) {
      return true;
    }
  }
  return false;
}

document.addEventListener("htmx:beforeRequest", function (event) {
  var list = event.detail.elt;
  if (list && list.id === "jobs" && playing(list)) {
    event.preventDefault();
  }
});

document.addEventListener("htmx:beforeSwap", function (event) {
  var list = event.detail.elt;
  if (list && list.id === "jobs" && playing(list)) {
    event.detail.shouldSwap = false;
  }
});

// The play button opens the player under its row and starts it; pressing it
// again hides and pauses it. The player keeps its state across refreshes
// (hx-preserve), so the button is set to match after each swap.
document.addEventListener("click", function (event) {
  var button = event.target.closest && event.target.closest("[data-play]");
  if (!button) {
    return;
  }
  var player = document.getElementById(button.getAttribute("data-play"));
  if (!player) {
    return;
  }
  var media = player.querySelector("audio, video");
  player.hidden = !player.hidden;
  button.setAttribute("aria-expanded", player.hidden ? "false" : "true");
  if (media) {
    if (player.hidden) {
      media.pause();
    } else {
      var started = media.play();
      if (started && started.catch) {
        started.catch(function () {});
      }
    }
  }
});

document.addEventListener("htmx:afterSettle", function () {
  var buttons = document.querySelectorAll("[data-play]");
  for (var i = 0; i < buttons.length; i++) {
    var player = document.getElementById(buttons[i].getAttribute("data-play"));
    buttons[i].setAttribute("aria-expanded", player && !player.hidden ? "true" : "false");
  }
});

// A menu closes when something outside it is clicked.
document.addEventListener("click", function (event) {
  var open = document.querySelectorAll("details.more[open]");
  for (var i = 0; i < open.length; i++) {
    if (!open[i].contains(event.target)) {
      open[i].open = false;
    }
  }
});

// After a subscription was added, fold the form away again.
document.addEventListener("htmx:afterRequest", function (event) {
  var form = event.target;
  if (!form.matches || !form.matches("form.add-sub-form")) {
    return;
  }
  var xhr = event.detail.xhr;
  if (event.detail.successful && xhr && !xhr.getResponseHeader("X-Ukai-Error")) {
    form.reset();
    var details = form.closest("details");
    if (details) {
      details.open = false;
    }
  }
});

// Removing a download or clearing the list asks first, in a dialog that
// deletes the downloaded files unless "Keep the files" is checked. The forms
// post files=1 only through it, so a form sent without scripts keeps them.
// The dialog sits outside the job list, which refreshes while anything runs,
// so it remembers the form by its address and finds it again on confirm.
var removing = null;

document.addEventListener("submit", function (event) {
  var form = event.target;
  if (!form.matches || !form.matches("form[data-remove]")) {
    return;
  }
  if (form.getAttribute("data-confirmed") === "1") {
    form.removeAttribute("data-confirmed");
    return;
  }
  var dialog = document.getElementById("remove-dialog");
  if (!dialog || !dialog.showModal) {
    return;
  }
  event.preventDefault();
  event.stopPropagation();
  var files = parseInt(form.getAttribute("data-files"), 10) > 0;
  removing = form.getAttribute("action");
  dialog.querySelector("h2").textContent = form.getAttribute("data-heading");
  dialog.querySelector(".what").textContent = form.getAttribute("data-what");
  dialog.querySelector(".files").hidden = !files;
  dialog.querySelector("input[name=keep]").checked = false;
  dialog.querySelector("button[value=confirm]").textContent = form.getAttribute("data-action");
  dialog.returnValue = "";
  dialog.showModal();
}, true);

document.addEventListener("close", function (event) {
  var dialog = event.target;
  if (dialog.id !== "remove-dialog") {
    return;
  }
  var action = removing;
  removing = null;
  if (dialog.returnValue !== "confirm" || !action) {
    return;
  }
  var forms = document.querySelectorAll("form[data-remove]");
  for (var i = 0; i < forms.length; i++) {
    if (forms[i].getAttribute("action") === action) {
      var keep = dialog.querySelector("input[name=keep]").checked;
      var files = parseInt(forms[i].getAttribute("data-files"), 10) > 0;
      forms[i].querySelector("input[name=files]").value = files && !keep ? "1" : "";
      forms[i].setAttribute("data-confirmed", "1");
      forms[i].requestSubmit();
      return;
    }
  }
}, true);

