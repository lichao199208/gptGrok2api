#!/bin/bash
python3 -m pip install -q curl_cffi 2>&1 | tail -3
python3 -c 'import curl_cffi; print("curl_cffi", curl_cffi.__version__)' 2>&1
