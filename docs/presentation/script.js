(() => {
  const slides = [...document.querySelectorAll('.slide')];
  const dots = document.querySelector('#slide-dots');
  const previous = document.querySelector('#previous');
  const next = document.querySelector('#next');
  const notes = document.querySelector('#notes-panel');
  const notesToggle = document.querySelector('#notes-toggle');
  const dialog = document.querySelector('#image-dialog');
  const controls = document.querySelector('#controls');
  const controlsToggle = document.querySelector('#controls-toggle');
  const controlsRestore = document.querySelector('#controls-restore');
  const durations = slides.map((slide) => Number(slide.dataset.duration));
  const total = durations.reduce((sum, seconds) => sum + seconds, 0);
  const formatTime = (seconds) =>
    `${String(Math.floor(seconds / 60)).padStart(2, '0')}:${String(seconds % 60).padStart(2, '0')}`;
  console.assert(
    slides.length === 9 && total === 330,
    'Presentation must have nine slides: five minutes plus one extra slide.',
  );
  let current = 0;

  function fit() {
    document.documentElement.style.setProperty(
      '--scale',
      Math.min(innerWidth / 1600, innerHeight / 900),
    );
  }

  function updateNotes() {
    const slide = slides[current];
    const before = durations.slice(0, current).reduce((sum, value) => sum + value, 0);
    document.querySelector('#notes-title').textContent = slide.dataset.title;
    document.querySelector('#notes-copy').textContent = slide.querySelector('.speech').textContent;
    document.querySelector('#notes-time').textContent =
      `Слайд ${current + 1} / ${slides.length} · ${durations[current]} сек. · план ${formatTime(before)}–${formatTime(before + durations[current])}`;
  }

  function show(index, updateHash = true) {
    current = Math.max(0, Math.min(slides.length - 1, index));
    slides.forEach((slide, i) => {
      slide.hidden = i !== current;
      slide.classList.toggle('active', i === current);
      dots.children[i].setAttribute('aria-current', String(i === current));
    });
    previous.disabled = current === 0;
    next.disabled = current === slides.length - 1;
    document.querySelector('#slide-number').innerHTML =
      `${String(current + 1).padStart(2, '0')} <span>/ ${String(slides.length).padStart(2, '0')}</span>`;
    if (updateHash) {
      try {
        history.replaceState(null, '', `#${current + 1}`);
      } catch {
        location.hash = String(current + 1);
      }
    }
    document.title = `${slides[current].dataset.title} — Knotra`;
    document.querySelector('#announcement').textContent =
      `Слайд ${current + 1} из ${slides.length}: ${slides[current].dataset.title}`;
    updateNotes();
  }

  function showHash() {
    const value = Number(location.hash.slice(1));
    show(Number.isInteger(value) && value >= 1 && value <= slides.length ? value - 1 : 0, false);
  }

  slides.forEach((slide, index) => {
    const dot = document.createElement('button');
    dot.type = 'button';
    dot.setAttribute('aria-label', `Слайд ${index + 1}: ${slide.dataset.title}`);
    dot.addEventListener('click', () => show(index));
    dots.append(dot);
  });

  function toggleNotes(force) {
    const open = typeof force === 'boolean' ? force : notes.hidden;
    notes.hidden = !open;
    notesToggle.setAttribute('aria-expanded', String(open));
    notesToggle.setAttribute(
      'aria-label',
      open ? 'Скрыть текст выступления' : 'Показать текст выступления',
    );
  }

  function toggleControls() {
    const hide = !controls.hidden;
    const hadFocus =
      controls.contains(document.activeElement) || document.activeElement === controlsRestore;
    controls.hidden = hide;
    document.querySelector('#keyboard-hint').hidden = hide;
    controlsRestore.hidden = !hide;
    controlsToggle.setAttribute('aria-expanded', String(!hide));
    if (hadFocus) (hide ? controlsRestore : controlsToggle).focus();
  }

  async function fullscreen() {
    try {
      if (document.fullscreenElement) await document.exitFullscreen();
      else if (document.documentElement.requestFullscreen)
        await document.documentElement.requestFullscreen();
      else
        document.querySelector('#fullscreen').title =
          'Используйте полноэкранный режим браузера: этот просмотрщик не поддерживает Fullscreen API';
    } catch {
      document.querySelector('#fullscreen').title =
        'Полный экран недоступен; используйте полноэкранный режим браузера';
    }
  }

  controlsToggle.addEventListener('click', toggleControls);
  controlsRestore.addEventListener('click', toggleControls);
  previous.addEventListener('click', () => show(current - 1));
  next.addEventListener('click', () => show(current + 1));
  notesToggle.addEventListener('click', () => toggleNotes());
  document.querySelector('#notes-close').addEventListener('click', () => toggleNotes(false));
  document.querySelector('#fullscreen').addEventListener('click', fullscreen);
  addEventListener('hashchange', showHash);
  addEventListener('resize', fit);

  document.addEventListener('keydown', (event) => {
    if (
      event.metaKey ||
      event.ctrlKey ||
      event.altKey ||
      dialog.open ||
      ['INPUT', 'TEXTAREA', 'SELECT'].includes(event.target.tagName)
    )
      return;
    if (event.code === 'Space' && event.target.closest('button, a')) return;
    const key = event.key.toLowerCase();
    if (['arrowright', 'arrowdown', 'pagedown', ' '].includes(key)) {
      event.preventDefault();
      show(current + 1);
    } else if (['arrowleft', 'arrowup', 'pageup'].includes(key)) {
      event.preventDefault();
      show(current - 1);
    } else if (key === 'home') {
      event.preventDefault();
      show(0);
    } else if (key === 'end') {
      event.preventDefault();
      show(slides.length - 1);
    } else if (event.code === 'KeyN') {
      event.preventDefault();
      toggleNotes();
    } else if (event.code === 'KeyH') {
      event.preventDefault();
      toggleControls();
    } else if (event.code === 'KeyF') {
      event.preventDefault();
      void fullscreen();
    } else if (key === 'escape') toggleNotes(false);
  });

  document.querySelectorAll('[data-image]').forEach((button) => {
    button.addEventListener('click', () => {
      const image = document.querySelector('#zoom-image');
      image.src = button.dataset.image;
      image.alt = button.dataset.caption;
      document.querySelector('#zoom-caption').textContent = button.dataset.caption;
      dialog.showModal();
    });
  });
  document.querySelector('#image-close').addEventListener('click', () => dialog.close());
  dialog.addEventListener('click', (event) => {
    if (event.target === dialog) dialog.close();
  });

  fit();
  showHash();
})();
