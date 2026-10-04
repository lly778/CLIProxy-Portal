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
      value = clamp(value);
      activate(value, true);
      // Fractional responsive widths can round the last slide's scroll limit
      // down by one pixel. Never wait for an unreachable scroll position.
      var left = Math.min(value * viewport.clientWidth, Math.max(0, viewport.scrollWidth - viewport.clientWidth));
      scrollingTo = Math.abs(viewport.scrollLeft - left) > 1 ? left : null;
      viewport.scrollTo({ left: left, behavior: animate && !reducedMotion ? "smooth" : "auto" });
    }
    function resized() { stopDrag(true); refreshSlides(); go(index, false); }
    function stopDrag(cancelled) {
      if (!drag) return;
      var finished = drag;
      drag = null;
      viewport.classList.remove("is-dragging");
      if (viewport.hasPointerCapture(finished.id)) viewport.releasePointerCapture(finished.id);
      if (!finished.moving) return;
      suppressClick = true;
      var threshold = Math.min(72, viewport.clientWidth * .15);
      var target = cancelled ? finished.index : Math.abs(finished.dx) >= threshold ? finished.index + (finished.dx < 0 ? 1 : -1) : finished.index;
      go(target, !cancelled);
    }
    listen(viewport, "scroll", function () {
      if (drag || !viewport.clientWidth) return;
      if (scrollingTo !== null) {
        if (Math.abs(viewport.scrollLeft - scrollingTo) > 1) return;
        scrollingTo = null;
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
      scrollingTo = null;
      if (slides.length < 2 || event.pointerType === "touch" || event.button !== 0 || event.target.closest("button, a, input, select, textarea, label")) return;
      drag = { id: event.pointerId, x: event.clientX, y: event.clientY, dx: 0, index: index, left: viewport.scrollLeft, moving: false };
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
    });
    listen(viewport, "pointerup", function () { stopDrag(false); });
    listen(viewport, "pointercancel", function () { stopDrag(true); });
    listen(viewport, "lostpointercapture", function () { stopDrag(true); });
    listen(viewport, "pointerleave", function () { if (drag && !drag.moving) drag = null; });
    listen(viewport, "click", function (event) {
      if (!suppressClick) return;
      suppressClick = false;
      event.preventDefault();
      event.stopPropagation();
    }, true);
    listen(window, "resize", resized);
    listen(viewport, "wheel", function () { scrollingTo = null; }, { passive: true });
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
        if (observer) observer.disconnect();
        removers.forEach(function (remove) { remove(); });
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
