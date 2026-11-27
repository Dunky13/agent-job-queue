#!/usr/bin/env python3
"""Writes packaging/dmg/DS_Store: the Finder window layout of the drag-to-Applications disk image.

The layout matches Kanarie's create-dmg settings: a 540 by 360 icon view with 128-point icons,
the app on the left and the Applications link on the right, with no toolbar, sidebar or
background image. It names entries only, never volume aliases, so the same file serves every build.
Needs ds_store: python3 -m pip install ds_store==1.3.1
Run: python3 packaging/dmg/generate-layout.py packaging/dmg/DS_Store
"""
import sys

from ds_store import DSStore

# Finder counts the window origin from the bottom-left of the screen; these are the values
# Kanarie's image stores for create-dmg's default top-left placement.
WIDTH, HEIGHT, LEFT, BOTTOM = 540, 360, 10, 697

with DSStore.open(sys.argv[1], "w+") as store:
    store["."]["bwsp"] = {
        "WindowBounds": "{{%d, %d}, {%d, %d}}" % (LEFT, BOTTOM, WIDTH, HEIGHT),
        "ShowSidebar": False, "ContainerShowSidebar": False, "SidebarWidth": 0,
        "ShowToolbar": False, "ShowStatusBar": False, "ShowPathbar": False, "ShowTabView": False,
        "PreviewPaneVisibility": False,
    }
    store["."]["icvp"] = {
        "viewOptionsVersion": 1, "iconSize": 128.0, "textSize": 16.0, "gridSpacing": 100.0,
        "gridOffsetX": 0.0, "gridOffsetY": 0.0, "labelOnBottom": True, "showItemInfo": False,
        "showIconPreview": True, "arrangeBy": "none", "backgroundType": 0,
        "backgroundColorRed": 1.0, "backgroundColorGreen": 1.0, "backgroundColorBlue": 1.0,
    }
    store["."]["vSrn"] = ("long", 1)
    store["Agent Queue.app"]["Iloc"] = (140, 170)
    store["Applications"]["Iloc"] = (400, 170)
