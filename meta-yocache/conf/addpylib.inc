# Pulled in by layer.conf ONLY on bitbake versions whose conf parser supports
# the `addpylib` directive (poky scarthgap/5.0+, where base.bbclass no longer
# auto-adds layer lib/ dirs to sys.path). On older bitbake (e.g. dunfell, bb
# 1.46) base.bbclass's oe_import() still does that, so this file is skipped and
# the bbclass's `from yocache import ...` resolves anyway. See layer.conf.
#
# LAYERDIR is still the layer root here: bitbake keeps it set for the whole
# layer.conf parse, and `include` is handled inline within that parse.
addpylib ${LAYERDIR}/lib yocache
