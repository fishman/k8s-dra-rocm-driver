#!/usr/bin/env bash

# Main intent: help users to self-troubleshoot when the GPU driver is not set up
# properly before installing this DRA driver. In that case, the log of the init
# container running this script is meant to yield an actionable error message.
# For now, rely on k8s to implement a high-level retry with back-off.

if [ -z "$ROCM_DRIVER_ROOT" ]; then
    # Not set, or set to empty string (not distinguishable).
    # Normalize to "/" (treated as such elsewhere).
    export ROCM_DRIVER_ROOT="/"
fi

# Remove trailing slash (if existing) and get last path element.
_driver_root_path="/driver-root-parent/$(basename "${ROCM_DRIVER_ROOT%/}")"

# Create in-container path /driver-root as a symlink. Expectation: link may be
# broken initially (e.g., if the GPU operator isn't deployed yet. The link heals
# once the driver becomes mounted (e.g., once GPU operator provides the driver
# on the host at /opt/rocm).
echo "create symlink: /driver-root -> ${_driver_root_path}"
ln -s "${_driver_root_path}" /driver-root

emit_common_err () {
    printf '%b' \
        "Check failed. Has the AMD/ROCm GPU driver been set up? " \
        "It is expected to be installed under " \
        "ROCM_DRIVER_ROOT (currently set to '${ROCM_DRIVER_ROOT}') " \
        "in the host filesystem. If that path appears to be unexpected: " \
        "review the DRA driver's 'rocmDriverRoot' Helm chart variable. " \
        "Otherwise, review if the GPU driver has " \
        "actually been installed under that path.\n"
}

validate_and_exit_on_success () {
    echo -n "$(date -u +"%Y-%m-%dT%H:%M:%SZ")  /driver-root (${ROCM_DRIVER_ROOT} on host): "

    # Search specific set of directories (not recursively: not required, and
    # /driver-root may be a big tree). Limit to first result (multiple results
    # are a bit of a pathological state, but continue with validation logic).
    # Suppress find stderr: some search directories are expected to be "not
    # found".

    ROCM_PATH=$( \
        find \
            /driver-root/opt/rocm/bin \
            /driver-root/usr/bin \
            /driver-root/usr/sbin \
            /driver-root/bin \
            /driver-root/sbin \
        -maxdepth 1 -type f -name "amdsmi" 2> /dev/null | head -n1
    )

    # Follow symlinks (-L), because ROCm libraries are typically links.
    # maxdepth 1 also protects against any potential symlink loop (we're
    # suppressing find's stderr, so we'd never see messages like 'Too many
    # levels of symbolic links').
    ROCM_LIB_PATH=$( \
        find -L \
            /driver-root/opt/rocm/lib64 \
            /driver-root/opt/rocm/lib \
            /driver-root/usr/lib64 \
            /driver-root/usr/lib/x86_64-linux-gnu \
            /driver-root/usr/lib/aarch64-linux-gnu \
            /driver-root/lib64 \
            /driver-root/lib/x86_64-linux-gnu \
            /driver-root/lib/aarch64-linux-gnu \
        -maxdepth 1 -type f -name "libamdsmi.so*" 2> /dev/null | head -n1
    )

    if [ -z "${ROCM_PATH}" ]; then
        echo -n "amdsmi: not found, "
    else
        echo -n "amdsmi: '${ROCM_PATH}', "
    fi

    if [ -z "${ROCM_LIB_PATH}" ]; then
        echo -n "libamdsmi.so: not found, "
    else
        echo -n "libamdsmi.so: '${ROCM_LIB_PATH}', "
    fi

    # Log top-level entries in /driver-root (this may be valuable debug info).
    echo "current contents: [$(/bin/ls -1xAw0 /driver-root 2>/dev/null)]."

    if [ -n "${ROCM_PATH}" ] && [ -n "${ROCM_LIB_PATH}" ]; then
        # Run with clean environment (only LD_PRELOAD; amdsmi has only this
        # dependency). Emit message before invocation (amdsmi may be slow or
        # hang).
        echo "invoke: env -i LD_PRELOAD=${ROCM_LIB_PATH} ${ROCM_PATH}"

        # Always show stderr, maybe hide or filter stdout?
        env -i LD_PRELOAD="${ROCM_LIB_PATH}" "${ROCM_PATH}"
        RCODE="$?"

        # For checking GPU driver health: rely on amdsmi's exit code. Rely
        # on code 0 signaling that the driver is properly set up.
        if [ ${RCODE} -eq 0 ]; then
            echo "amdsmi returned with code 0: success, leave"

            # Exit script indicating success (leave init container).
            exit 0
        fi
        echo "exit code: ${RCODE}"
    fi

    # Reduce log volume: log hints only every Nth attempt.
    if [ $((_ATTEMPT % 6)) -ne 0 ]; then
        return
    fi

    # amdsmi binaries not found, or execution failed. First, provide generic
    # error message. Then, try to provide actionable hints for common problems.
    echo
    emit_common_err

    # For host-provided driver not at / provide feedback for two special cases.
    if [ "${ROCM_DRIVER_ROOT}" != "/" ]; then
        if [ -z "$( ls -A /driver-root )" ]; then
            echo "Hint: Directory $ROCM_DRIVER_ROOT on the host is empty"
        else
            # Not empty, but at least one of the binaries not found: this is a
            # rather pathological state.
            if [ -z "${ROCM_PATH}" ] || [ -z "${ROCM_LIB_PATH}" ]; then
                echo "Hint: Directory $ROCM_DRIVER_ROOT is not empty but at least one of the binaries wasn't found."
            fi
        fi
    fi

    # Common mistake: driver container, but forgot `--set rocmDriverRoot`
    if [ "${ROCM_DRIVER_ROOT}" == "/" ] && [ -f /driver-root/opt/rocm/bin/amdsmi ]; then
        printf '%b' \
        "Hint: '/opt/rocm/bin/amdsmi' exists on the host, you " \
        "may want to re-install the DRA driver Helm chart with " \
        "--set rocmDriverRoot=/opt/rocm\n"
    fi

    if [ "${ROCM_DRIVER_ROOT}" == "/opt/rocm" ]; then
        printf '%b' \
            "Hint: ROCM_DRIVER_ROOT is set to '/opt/rocm' " \
            "which typically means that the ROCm driver " \
            "is installed at this location. Make sure that ROCm " \
            "is properly installed and healthy.\n"
    fi
    echo
}

# DS pods may get deleted (terminated with SIGTERM) and re-created when the ROCm
# driver becomes available. Make that explicit.
log_sigterm() {
  echo "$(date -u +"%Y-%m-%dT%H:%M:%S.%3NZ"): received SIGTERM"
  exit 0
}
trap 'log_sigterm' SIGTERM


# Design goal: long-running init container that retries at constant frequency,
# and leaves only upon success (with code 0).
_WAIT_S=10
_ATTEMPT=0

while true
do
    validate_and_exit_on_success
    sleep ${_WAIT_S}
    _ATTEMPT=$((_ATTEMPT+1))
done
