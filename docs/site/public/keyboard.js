export function keyboardAction(event, { dialog = null, editable = false } = {}) {
  if (event.isComposing || event.keyCode === 229 || event.repeat || event.altKey) return null;
  if (!event.shiftKey && (event.metaKey || event.ctrlKey) && event.code === 'KeyK') {
    return !dialog ? 'search' : dialog === 'search-dialog' ? 'search-focus' : 'blocked';
  }
  if (dialog && event.code === 'Escape' && !event.metaKey && !event.ctrlKey) return 'dialog-close';
  if (dialog === 'search-dialog') {
    if (!editable) return null;
    if (event.metaKey || event.ctrlKey) return null;
    if (event.code === 'ArrowDown') return 'search-next';
    if (event.code === 'ArrowUp') return 'search-previous';
    if (event.code === 'Enter') return 'search-open';
    return null;
  }
  if (dialog === 'diagram-dialog' && !editable && !event.metaKey && !event.ctrlKey) {
    if (event.code === 'Equal' || event.code === 'NumpadAdd') return 'zoom-in';
    if (event.code === 'Minus' || event.code === 'NumpadSubtract') return 'zoom-out';
    if (event.code === 'Digit0' || event.code === 'Numpad0') return 'zoom-reset';
    if (event.code === 'KeyW') return 'zoom-width';
  }
  return null;
}
