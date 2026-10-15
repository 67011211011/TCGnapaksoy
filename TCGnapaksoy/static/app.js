// 1) นับถอยหลังการประมูล (แสดงผลเท่านั้น — การตัดสินว่าปิดประมูลหรือยังเป็นหน้าที่ของเซิร์ฟเวอร์เสมอ)
//    ใช้ "เวลาที่เหลือ" จากเซิร์ฟเวอร์ ไม่ใช้นาฬิกาของเครื่องผู้ใช้ จึงไม่เพี้ยนถ้านาฬิกาเครื่องผิด
(function () {
  var timers = document.querySelectorAll('.timer[data-remaining]');
  if (!timers.length) return;
  var start = Date.now();

  function pad(n) { return (n < 10 ? '0' : '') + n; }

  function tick() {
    var elapsed = Math.floor((Date.now() - start) / 1000);
    timers.forEach(function (el) {
      var left = parseInt(el.getAttribute('data-remaining'), 10) - elapsed;
      if (!(left > 0)) {
        el.textContent = 'ปิดประมูลแล้ว (รีเฟรชเพื่อดูผล)';
        el.classList.add('closed');
        return;
      }
      var d = Math.floor(left / 86400);
      var h = Math.floor((left % 86400) / 3600);
      var m = Math.floor((left % 3600) / 60);
      var s = left % 60;
      el.innerHTML =
        '<span><b>' + pad(d) + '</b>วัน</span>' +
        '<span><b>' + pad(h) + '</b>ชั่วโมง</span>' +
        '<span><b>' + pad(m) + '</b>นาที</span>' +
        '<span><b>' + pad(s) + '</b>วินาที</span>';
    });
  }

  tick();
  setInterval(tick, 1000);
})();

// 2) พรีวิวรูปที่เลือกก่อนบันทึก (แสดงผลเท่านั้น — เซิร์ฟเวอร์ตรวจและประมวลผลไฟล์จริงเสมอ)
(function () {
  var inputs = document.querySelectorAll('input[type=file][data-preview]');
  inputs.forEach(function (input) {
    input.addEventListener('change', function () {
      var file = input.files && input.files[0];
      var box = input.closest('.drop');
      if (!file || !box) return;
      var img = box.querySelector('.pv');
      var hint = box.querySelector('.drop-hint');
      if (file.size > 2 * 1024 * 1024) {
        if (hint) hint.textContent = '⚠️ ไฟล์ใหญ่เกิน 2MB กรุณาเลือกรูปใหม่';
        input.value = '';
        return;
      }
      if (img) {
        img.src = URL.createObjectURL(file);
        img.classList.remove('empty');
      }
      if (hint) hint.textContent = '✅ เลือกแล้ว: ' + file.name;
    });
  });
})();
