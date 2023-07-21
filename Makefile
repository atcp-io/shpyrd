#!/bin/bash
operator:
	ARCH=$(case $(uname -m) in x86_64) echo -n amd64 ;; aarch64) echo -n arm64 ;; *) echo -n $(uname -m) ;; esac)
	OS=$(uname | awk '{print tolower($0)}')
	OPERATOR_SDK_DL_URL=https://github.com/operator-framework/operator-sdk/releases/download/v1.30.0
	curl -LO ${OPERATOR_SDK_DL_URL}/operator-sdk_${OS}_${ARCH}
	mkdir -p bin
	mv operator-sdk_${OS}_${ARCH} bin/operator-sdk
	chmod 755 bin/operator-sdk