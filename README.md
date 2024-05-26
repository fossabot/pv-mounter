# pv-mounter 

A tool to locally mount k8s PVs using SSHFS.

Might be used as `kubectl` plugin.

## Disclaimer

This tool was created with huge help of [ChatGPT-4o](https://chatgpt.com/?model=gpt-4o) and [perplexity](https://www.perplexity.ai/).
In fact I didn't have to write my own code almost at all but I had to spend a lot of time writing correct prompts for these tools.

**Update**

Above was true for versions 0.0.x. With version 0.5.0 I actually had to learn some Go and while I was still using help from GPT I had to completely change approach. 
It wasn't able to create fully functional code with all my requirements. 

I published it using Apache-2.0 license cause initial [repository](https://github.com/replicatedhq/krew-plugin-template) was licensed this way but to be honest I'm not sure how such copy&paste stuff should be licensed.

## Rationale

I often need to copy some files from my [homelab](https://github.com/fenio/homelab) which is running on k8s. Having ability to work on these files locally greatly simplifies this task.
Thus pv-mounter was born to automate that process.

## What exactly does it do?

Few things. In case of volumes with RWX access mode or unmounted RWO:

* spawns a POD with minimalistic image that contains SSH daemon and binds to existing PVC
* creates port-forward to make it locally accessible
* mounts volume locally using SSHFS 

In case of already mounted RWO volumes it's a bit more complex:

* spawns a POD with minimalistic image that contains SSH daemon and will act as proxy to ephemeral container
* creates ephemeral container within POD that currently mounts volume
* from that ephemeral container establishes reverse SSH tunnel to to proxy POD
* creates port-forward to proxy POD onto port exposed by tunnel to make it locally accessible
* mounts volume locally using SSHFS

See also demo below.

## Prerequisities

* You need working SSHFS setup.

Instructions for [macOS](https://osxfuse.github.io/).
Instructions for [Linux](https://github.com/libfuse/sshfs).

## Quick Start

```
kubectl krew install pv-mounter

kubectl pv-mounter mount <namespace> <pvc-name> <local-mountpoint>
kubectl pv-mounter clean <namespace> <pvc-name> <local-mountpoint>

```

Obviously you have to have working [krew](https://krew.sigs.k8s.io/docs/user-guide/setup/install/) first.

Or you can simply grab binaries from [releases](https://github.com/fenio/pv-mounter/releases).

## Limitations

Tool has clean option that does its best to clean up all stuff it created for mounting volume locally. But ephemeral containers can't be removed/deleted. That's the way k8s works. Thus as part of cleanup tool kills process that keeps that ephemeral container alive. I confirmed it also kills other processes that were running on that container but container itself stays in pretty weird state.

## Demo

![Demo](demo.gif)


### Windows

Since I can't test Windows binaries they're now simply not included but I saw there is SSHFS implementation for Windows so in theory this should work.

## FAQ

Ask questions first ;)
