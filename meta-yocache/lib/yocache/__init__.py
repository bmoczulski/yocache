# yocache build-side Python package (wired onto sys.path via `addpylib` in
# meta-yocache/conf/layer.conf). Holds code that must keep real, process-lived
# state across bitbake event-handler calls — currently the artifact uploader.
