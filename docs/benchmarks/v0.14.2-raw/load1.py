#!/usr/bin/env python3
"""load1.py — print the 1-minute load average as a plain float.

Locale-proof on purpose. This host is pt_PT, so `uptime` prints "10,01" and the
obvious shell one-liners are wrong in two different ways:
  * awk compares "10.01" > "2.0" as STRINGS (a '.' field is not numeric under a
    comma-radix locale), so the comparison silently returns false; and
  * a bare tr ',' '.' fixes the text but not awk's numeric test.
A load gate built on either would never fire, and the report would claim an idle
host it never checked. Read via os.getloadavg(), which has no text step at all.
"""
import os
print(f"{os.getloadavg()[0]:.2f}")
