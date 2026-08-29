#!/bin/bash
echo "which python3: $(which python3)"
echo "ls /app: $(ls /app 2>/dev/null | head -5)"
echo "pip: $(pip --version 2>&1 | head -1)"
echo "sys.prefix: $(python3 -c 'import sys; print(sys.prefix)' 2>&1)"
echo "PYTHONPATH: $PYTHONPATH"
echo "ls /opt: $(ls /opt 2>/dev/null | head)"
python3 - <<'EOF'
import sys
print("sys.path:", sys.path)
try:
    import fastapi
    print("fastapi ok")
except Exception as e:
    print("fastapi missing:", e)
try:
    import requests
    print("requests ok")
except Exception as e:
    print("requests missing:", e)
EOF
