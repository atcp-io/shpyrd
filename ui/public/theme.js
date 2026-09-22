// Apply the stored theme before the first paint (see src/lib/theme.ts).
// A separate file rather than an inline script so the dashboard's Content
// Security Policy needs no 'unsafe-inline' for scripts.
(function () {
  var t = localStorage.getItem('shpyrd.theme')
  var dark = t === 'dark' || (t !== 'light' && window.matchMedia('(prefers-color-scheme: dark)').matches)
  if (dark) document.documentElement.classList.add('dark')
})()
