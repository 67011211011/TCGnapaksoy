#!/bin/sh
# volume ที่ mount เข้ามามักเป็นของ root → ปรับสิทธิ์ แล้วรันแอปด้วยผู้ใช้ที่ไม่ใช่ root
chown -R app /data
exec su-exec app /app
