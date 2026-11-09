include $(TOPDIR)/rules.mk

PKG_NAME:=luci-app-sms-archive
PKG_VERSION:=$(or $(SMS_ARCHIVE_VERSION),0.1.0)
PKG_RELEASE:=$(or $(SMS_ARCHIVE_RELEASE),1)
PKG_LICENSE:=MIT
PKG_LICENSE_FILES:=LICENSE

include $(INCLUDE_DIR)/package.mk

define Package/luci-app-sms-archive
  SECTION:=luci
  CATEGORY:=LuCI
  SUBMENU:=3. Applications
  TITLE:=SMS archive for USB modems
  EXTRA_DEPENDS:=luci-base >=0, rpcd-mod-ucode >=0, ucode-mod-fs >=0, ucode-mod-uci >=0, ucode-mod-ubus >=0, kmod-usb-acm >=0
endef

define Package/luci-app-sms-archive/description
  Archive SMS from SIM and modem storage on the router and browse them in LuCI.
endef

define Build/Prepare
	mkdir -p $(PKG_BUILD_DIR)
endef

define Build/Compile
	$(INSTALL_BIN) ./sms-archive $(PKG_BUILD_DIR)/sms-archive
endef

define Package/luci-app-sms-archive/install
	$(CP) ./files/. $(1)/
	find $(1) -type d -exec chmod 0755 '{}' +
	find $(1) -type f -exec chmod 0644 '{}' +
	$(INSTALL_DIR) $(1)/usr/sbin $(1)/usr/share/sms-archive $(1)/etc/uci-defaults
	$(INSTALL_BIN) $(PKG_BUILD_DIR)/sms-archive $(1)/usr/sbin/sms-archive
	$(INSTALL_DATA) ./LICENSE $(1)/usr/share/sms-archive/LICENSE
	$(INSTALL_DATA) ./THIRD-PARTY-LICENSES $(1)/usr/share/sms-archive/THIRD-PARTY-LICENSES
	$(INSTALL_DATA) ./files/etc/config/sms_archive $(1)/usr/share/sms-archive/default.config
	rm -f $(1)/etc/config/sms_archive
	$(INSTALL_BIN) ./uci-defaults $(1)/etc/uci-defaults/90-sms-archive
	chmod 0755 $(1)/etc/init.d/sms-archive
endef

define Package/luci-app-sms-archive/preinst
#!/bin/sh
if [ -z "$${IPKG_INSTROOT}" ] && [ "$${PKG_UPGRADE}" = 1 ] && \
	[ -x /etc/init.d/sms-archive ] && /etc/init.d/sms-archive running; then
	/etc/init.d/sms-archive stop
fi
endef

define Package/luci-app-sms-archive/postinst
#!/bin/sh
if [ -z "$${IPKG_INSTROOT}" ] && [ -x /etc/init.d/rpcd ]; then
	/etc/init.d/rpcd restart
fi
endef

define Package/luci-app-sms-archive/postrm
#!/bin/sh
if [ -z "$${IPKG_INSTROOT}" ] && [ -x /etc/init.d/rpcd ]; then
	/etc/init.d/rpcd restart
	rm -f /tmp/luci-indexcache.*
fi
endef

$(eval $(call BuildPackage,luci-app-sms-archive))
