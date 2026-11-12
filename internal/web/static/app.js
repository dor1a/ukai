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
  }
});
