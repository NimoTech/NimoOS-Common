/*@Author: LinkLeong link@icewhale.com
 *@Date: 2022-05-27 15:01:58
 *@LastEditors: LinkLeong
 *@LastEditTime: 2022-05-31 14:51:21
 *@FilePath: /NimoOS/model/notify/application.go
 *@Description:
 *Copyright (c) 2021-2025 IceWhale Technology Co., Ltd.
 *Copyright (c) 2026 NimoTech
 *Licensed under the Apache License, Version 2.0.
 *Modified from the original CasaOS source by NimoTech.
 */
package notify

type Application struct {
	Name       string            `json:"name"`
	State      string            `json:"state"`
	Type       string            `json:"type"`
	Icon       string            `json:"icon"`
	Message    string            `json:"message"`
	Finished   bool              `json:"finished"`
	Success    bool              `json:"success"`
	Properties map[string]string `json:"properties"`
}
