(function () {
  "use strict";

  function init(root, preferredProvider) {
    if (!root || root.quotaCarousel) return;
    var viewport = root.querySelector("[data-quota-viewport]");
    var track = root.querySelector("[data-quota-track]");
    if (!viewport || !track) return;
    var allSlides = Array.prototype.slice.call(track.querySelectorAll("[data-quota-provider]"));
    if (!allSlides.length) return;
    var slides = [];
    var controls = root.querySelector("[data-quota-controls]");
    var previous = root.querySelector("[data-quota-previous]");
    var next = root.querySelector("[data-quota-next]");
    var buttons = Array.prototype.slice.call(root.querySelectorAll("[data-quota-select]"));
    var status = root.querySelector("[data-quota-carousel-status]");
    var index = 0;
    var drag = null;
    var scrollingTo = null;
    var animationFrame = null;
    var suppressClick = false;
    var removers = [];
    var rememberedVariants = Object.create(null);
    var reducedMotion = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    function listen(target, event, handler, options) {
      if (!target) return;
      target.addEventListener(event, handler, options);
      removers.push(function () { target.removeEventListener(event, handler, options); });
    }
    function clamp(value) { return Math.max(0, Math.min(slides.length - 1, value)); }
    function now() { return window.performance && window.performance.now ? window.performance.now() : Date.now(); }
    function cancelAnimation() {
      if (animationFrame !== null) window.cancelAnimationFrame(animationFrame);
      animationFrame = null;
    }
    function size() { viewport.style.height = slides[index].offsetHeight + "px"; }
    function refreshSlides(preferred) {
      var old = slides[index] || allSlides.find(function (slide) { return slide.getAttribute("data-quota-provider") === preferred; });
      var key = preferred || old && old.getAttribute("data-quota-provider");
      var channel = old && (old.getAttribute("data-quota-channel") || old.getAttribute("data-quota-provider"));
      slides = allSlides.filter(function (slide) {
        return !slide.getAttribute("data-quota-layout") || window.getComputedStyle(slide).display !== "none";
      });
      index = slides.findIndex(function (slide) { return slide.getAttribute("data-quota-provider") === key; });
      if (index < 0 && channel) index = slides.findIndex(function (slide) { return slide.getAttribute("data-quota-provider") === rememberedVariants[channel]; });
      if (index < 0 && channel) index = slides.findIndex(function (slide) { return slide.getAttribute("data-quota-channel") === channel; });
      if (index < 0) index = 0;
      if (controls) controls.hidden = slides.length < 2;
    }
    refreshSlides(preferredProvider);
    function activate(value, announce) {
      var changed = index !== value;
      index = value;
      allSlides.forEach(function (slide) {
        slide.inert = slide !== slides[index];
        slide.setAttribute("aria-hidden", slide === slides[index] ? "false" : "true");
      });
      if (slides[index].getAttribute("data-quota-layout") === "narrow") rememberedVariants[slides[index].getAttribute("data-quota-channel")] = slides[index].getAttribute("data-quota-provider");
      buttons.forEach(function (button) {
        button.setAttribute("aria-pressed", button.getAttribute("data-quota-select") === slides[index].getAttribute("data-quota-provider") ? "true" : "false");
      });
      if (previous) previous.hidden = previous.disabled = index === 0;
      if (next) next.hidden = next.disabled = index === slides.length - 1;
      size();
      if (status && announce && changed) status.textContent = slides[index].getAttribute("data-quota-provider") + "，第 " + (index + 1) + " / " + slides.length + " 张额度卡片";
    }
    function go(value, animate) {
      cancelAnimation();
      value = clamp(value);
      activate(value, true);
      // Fractional responsive widths can round the last slide's scroll limit
      // down by one pixel. Never wait for an unreachable scroll position.
      var left = Math.min(value * viewport.clientWidth, Math.max(0, viewport.scrollWidth - viewport.clientWidth));
      scrollingTo = Math.abs(viewport.scrollLeft - left) > 1 ? left : null;
      viewport.scrollTo({ left: left, behavior: animate && !reducedMotion ? "smooth" : "auto" });
      if (scrollingTo === null) viewport.classList.remove("is-settling");
    }
    function settle(value, finished) {
      if (reducedMotion || !window.requestAnimationFrame || !window.cancelAnimationFrame) { go(value, !reducedMotion); return; }
      cancelAnimation();
      value = clamp(value);
      var start = viewport.scrollLeft;
      var left = Math.min(value * viewport.clientWidth, Math.max(0, viewport.scrollWidth - viewport.clientWidth));
      var distance = left - start;
      if (Math.abs(distance) <= 1) { go(value, false); return; }
      var started = now();
      var first = finished.samples[0], last = finished.samples[finished.samples.length - 1];
      var elapsed = started - first.time;
      var velocity = elapsed > 0 && started - last.time < 80 ? (last.left - first.left) / elapsed : 0;
      var speed = Math.max(0, velocity * (distance < 0 ? -1 : 1));
      var duration = Math.min(360, 180 + Math.abs(distance) * .22);
      if (speed > 0) duration = Math.max(16, Math.min(duration, 2.5 * Math.abs(distance) / speed));
      var slope = Math.min(3, speed * duration / Math.abs(distance));
      activate(value, true);
      scrollingTo = left;
      viewport.classList.add("is-settling");
      // Hermite interpolation carries the release velocity into the first frame
      // and slows to zero at the target, without a new native ease-in phase.
      function frame(time) {
        var t = Math.max(0, Math.min(1, (time - started) / duration));
        var progress = (3 - 2 * t) * t * t + slope * t * (1 - t) * (1 - t);
        viewport.scrollLeft = start + distance * progress;
        if (t < 1) { animationFrame = window.requestAnimationFrame(frame); return; }
        animationFrame = null;
        scrollingTo = null;
        viewport.classList.remove("is-settling");
        activate(value, false);
      }
      animationFrame = window.requestAnimationFrame(frame);
    }
    function resized() { stopDrag(true); refreshSlides(); go(index, false); }
    function stopDrag(cancelled) {
      if (!drag) return;
      var finished = drag;
      drag = null;
      // Re-enabling native snapping here would first jump to the nearest card.
      // Keep it off until our animation reaches its target from the release point.
      if (finished.moving) viewport.classList.add("is-settling");
      viewport.classList.remove("is-drag-pending");
      viewport.classList.remove("is-dragging");
      if (viewport.hasPointerCapture(finished.id)) viewport.releasePointerCapture(finished.id);
      if (!finished.moving) { viewport.classList.remove("is-settling"); return; }
      suppressClick = true;
      var threshold = Math.min(72, viewport.clientWidth * .15);
      var target = cancelled ? finished.index : Math.abs(finished.dx) >= threshold ? finished.index + (finished.dx < 0 ? 1 : -1) : finished.index;
      if (cancelled) go(target, false);
      else settle(target, finished);
    }
    listen(viewport, "scroll", function () {
      if (drag || animationFrame !== null || !viewport.clientWidth) return;
      if (scrollingTo !== null) {
        if (Math.abs(viewport.scrollLeft - scrollingTo) > 1) return;
        scrollingTo = null;
        viewport.classList.remove("is-settling");
      }
      activate(clamp(Math.round(viewport.scrollLeft / viewport.clientWidth)), true);
    });
    listen(previous, "click", function () { go(index - 1, true); });
    listen(next, "click", function () { go(index + 1, true); });
    buttons.forEach(function (button) {
      listen(button, "click", function () {
        go(slides.findIndex(function (slide) { return slide.getAttribute("data-quota-provider") === button.getAttribute("data-quota-select"); }), true);
      });
    });
    listen(viewport, "keydown", function (event) {
      if (event.target !== viewport) return;
      var target;
      if (event.key === "ArrowLeft") target = index - 1;
      else if (event.key === "ArrowRight") target = index + 1;
      else if (event.key === "Home") target = 0;
      else if (event.key === "End") target = slides.length - 1;
      else return;
      event.preventDefault();
      go(target, true);
    });
    // Touch scrolling stays native, including vertical page scroll and pinch zoom.
    listen(viewport, "pointerdown", function (event) {
      suppressClick = false;
      if (event.pointerType === "touch") {
        if (viewport.classList.contains("is-settling")) {
          cancelAnimation();
          viewport.scrollTo({ left: viewport.scrollLeft, behavior: "auto" });
          scrollingTo = null;
          viewport.classList.remove("is-settling");
        }
        return;
      }
      // Text uses native selection; only blank card space starts carousel dragging.
      if (slides.length < 2 || event.button !== 0 || event.target.closest("button, a, input, select, textarea, label, [data-quota-text]")) return;
      // Lock the gesture at its blank-space origin, before the drag threshold.
      // Otherwise the browser can start selection as the pointer crosses text.
      event.preventDefault();
      if (viewport.focus) viewport.focus({ preventScroll: true });
      viewport.classList.add("is-drag-pending");
      cancelAnimation();
      scrollingTo = null;
      if (viewport.classList.contains("is-settling")) {
        viewport.scrollTo({ left: viewport.scrollLeft, behavior: "auto" });
      }
      drag = { id: event.pointerId, x: event.clientX, y: event.clientY, dx: 0, index: index, left: viewport.scrollLeft, moving: false, samples: [{ left: viewport.scrollLeft, time: now() }] };
    });
    listen(viewport, "pointermove", function (event) {
      if (!drag || event.pointerId !== drag.id) return;
      drag.dx = event.clientX - drag.x;
      if (!drag.moving) {
        if (Math.abs(drag.dx) < 8 || Math.abs(drag.dx) <= Math.abs(event.clientY - drag.y)) return;
        drag.moving = true;
        viewport.classList.add("is-dragging");
        viewport.setPointerCapture(drag.id);
      }
      event.preventDefault();
      viewport.scrollLeft = drag.left - drag.dx;
      var time = now();
      drag.samples.push({ left: viewport.scrollLeft, time: time });
      while (drag.samples.length > 2 && drag.samples[1].time < time - 80) drag.samples.shift();
    });
    listen(viewport, "pointerup", function () { stopDrag(false); });
    listen(viewport, "pointercancel", function () { stopDrag(true); });
    listen(viewport, "lostpointercapture", function () { stopDrag(true); });
    listen(viewport, "pointerleave", function () { if (drag && !drag.moving) stopDrag(true); });
    listen(viewport, "selectstart", function (event) { if (drag) event.preventDefault(); });
    listen(window, "blur", function () { stopDrag(true); });
    listen(viewport, "click", function (event) {
      if (!suppressClick) return;
      suppressClick = false;
      event.preventDefault();
      event.stopPropagation();
    }, true);
    listen(window, "resize", resized);
    listen(viewport, "wheel", function () { cancelAnimation(); scrollingTo = null; viewport.classList.remove("is-settling"); }, { passive: true });
    var lastWidth = viewport.clientWidth;
    var observer = window.ResizeObserver ? new window.ResizeObserver(function () {
      if (lastWidth !== viewport.clientWidth) { lastWidth = viewport.clientWidth; resized(); }
      else size();
    }) : null;
    if (observer) { observer.observe(root); allSlides.forEach(function (slide) { observer.observe(slide); }); }
    if (controls) controls.hidden = slides.length < 2;
    root.quotaCarousel = {
      selected: function () { return slides[index].getAttribute("data-quota-provider"); },
      destroy: function () {
        cancelAnimation();
        if (observer) observer.disconnect();
        removers.forEach(function (remove) { remove(); });
        viewport.classList.remove("is-drag-pending");
        viewport.classList.remove("is-dragging");
        viewport.classList.remove("is-settling");
        delete root.quotaCarousel;
      }
    };
    go(index, false);
  }
  window.QuotaCarousel = {
    init: init,
    selected: function (root) { return root && root.quotaCarousel ? root.quotaCarousel.selected() : ""; },
    destroy: function (root) { if (root && root.quotaCarousel) root.quotaCarousel.destroy(); }
  };
  document.querySelectorAll("[data-quota-pool]").forEach(function (root) { init(root); });
})();
